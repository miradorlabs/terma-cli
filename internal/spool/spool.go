// Package spool is the local, append-only event queue every hook writes to: a hook
// appends one JSON line under the state directory and exits, and `terma spool flush`
// delivers later, so no hook ever waits on the backend.
package spool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Event is one thing that happened on this machine worth telling the backend.
type Event struct {
	Time      time.Time `json:"time"`
	Name      string    `json:"name"`
	SessionID string    `json:"session_id,omitempty"`
	// TraceID is the OTel trace the event belongs to, 32 lowercase hex digits; it leaves
	// in the log record's TraceId field.
	TraceID string `json:"trace_id,omitempty"`
	// Global records machine-wide capture, so queued events can be withheld after a
	// switch back to repository coverage. Local only.
	Global bool `json:"global,omitempty"`
	// Repository is the working copy as admission names it, so delivery holds a queued
	// event to the repository list in force when it leaves. Local only.
	Repository config.Repository `json:"repository,omitzero"`
	Attrs      map[string]any    `json:"attrs,omitempty"`

	// The destination multi-org routing gave the event when it was spooled, which is
	// fixed: delivery rechecks it before sending and never routes the event again. An
	// event without these is routed by attrs.project_id, as before. Local only.

	// Profile is the profile whose routing state routed the event.
	Profile string `json:"profile,omitempty"`
	// Enrollment is the event's destination: the enrollment it is sent to, or the one
	// whose team holds it.
	Enrollment string `json:"enrollment,omitempty"`
	// HoldKey is the pending binding holding the event, which is sent only if that binding
	// is then bound to the holding team's organization.
	HoldKey string `json:"hold_key,omitempty"`
	// Run is the session run the event belongs to (internal/runs).
	Run string `json:"run,omitempty"`
	// OwnLocation is set when the event's location is its own, as a file event's checkout
	// or the directory an agent's turn names is, rather than its run's. Its run turning
	// Mixed still drops it unless it carries no content (runs.Locational).
	OwnLocation bool `json:"own_location,omitempty"`
}

// ValidTraceID reports whether id is an OTel trace id: 32 lowercase hex digits, not all zero.
func ValidTraceID(id string) bool {
	if len(id) != 32 || id == strings.Repeat("0", 32) {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}

// Sender delivers a batch. Send must respect context cancellation.
type Sender interface {
	// Send returns as held the events it cannot deliver yet (no project key), which
	// re-queue at the tail. A *PartialDelivery means only its Undelivered failed; any
	// other error means nothing was accepted.
	Send(ctx context.Context, events []Event) (held []Event, err error)
}

// PartialDelivery is a Sender's error when some destinations accepted their part of
// a batch and others failed; it opens no spool-wide backoff (see DestinationFailed).
type PartialDelivery struct {
	Undelivered []Event
	// Err is nil when the sender only deferred events to a destination already
	// failing, which leaves work queued without failing the pass.
	Err error
}

func (p *PartialDelivery) Error() string {
	if p.Err == nil {
		return fmt.Sprintf("%d event(s) deferred after an earlier failure", len(p.Undelivered))
	}
	return p.Err.Error()
}

func (p *PartialDelivery) Unwrap() error { return p.Err }

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, events []Event) ([]Event, error)

// Send calls f.
func (f SenderFunc) Send(ctx context.Context, events []Event) ([]Event, error) {
	return f(ctx, events)
}

// Dir is the spool's folder under the state directory.
const Dir = "spool"

// DeliveryLockFile is the lock, in Dir, every flush holds while it delivers: whoever holds
// it knows no flush is sending, as the migration to multi-org routing must
// (internal/migrate).
const DeliveryLockFile = "delivery.lock"

const (
	eventsFile       = "events.jsonl"
	backoffFile      = "next-attempt"
	lastFlushFile    = "last-flush"
	lockFile         = "flush.lock" // shared by appends and queue rewrites
	deliveryLockFile = DeliveryLockFile
	// probeEventName is reserved for Writable's probe line, which never reaches a sender.
	probeEventName = "terma-write-probe"
	// MaxBytes bounds the spool; past it the next flush drops the oldest events.
	MaxBytes = 16 << 20
	// MaxAge is how long an undelivered event is kept; older ones are dropped and counted.
	MaxAge = 14 * 24 * time.Hour
	// DefaultBatch is how many events go in one send.
	DefaultBatch = 200
	// DefaultFlushTimeout bounds a whole delivery pass, including lock waits.
	DefaultFlushTimeout = 30 * time.Second
	// Local operations must not wait indefinitely for another process.
	localLockTimeout = 250 * time.Millisecond
	minBackoff       = 30 * time.Second
	maxBackoff       = time.Hour
	dirMode          = 0o700
	fileMode         = 0o600
)

// Spool is one queue directory.
type Spool struct {
	dir string
}

// Open addresses (and creates) the spool directory.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) path() string { return filepath.Join(s.dir, eventsFile) }

// Append records events in one write under the lock Flush holds while rewriting the file,
// so they never land on a file about to be replaced.
func (s *Spool) Append(events ...Event) error {
	return s.AppendContext(context.Background(), events...)
}

// AppendContext observes caller cancellation and caps lock waits even when the
// caller has no deadline. An error means no event was queued.
func (s *Spool) AppendContext(ctx context.Context, events ...Event) error {
	var lines []byte
	for _, e := range events {
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		lines = append(append(lines, line...), '\n')
	}
	// The budget starts after the encode: a slow encode charged to the wait would
	// refuse a lock nobody holds and lose the event.
	lockCtx, cancel := context.WithTimeout(ctx, localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(lockCtx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(lines)
	return err
}

// Pending reports how many events are queued and the spool size in bytes.
func (s *Spool) Pending() (count int, size int64, err error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return bytes.Count(data, []byte{'\n'}), int64(len(data)), nil
}

// Writable reports whether an event would land, by appending a probe line under the
// append lock and truncating it off; a read-only or full disk still reads as healthy.
func (s *Spool) Writable() error {
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()

	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	probe, err := json.Marshal(Event{Name: probeEventName})
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(append(probe, '\n'), info.Size()); err != nil {
		return err
	}
	// A full disk can accept a write and refuse it only at sync.
	if err := f.Sync(); err != nil {
		_ = f.Truncate(info.Size()) // best effort: leave the queue as it was
		return err
	}
	if err := f.Truncate(info.Size()); err != nil {
		return err
	}
	return f.Sync()
}

// Sessions calls add with the session ID of every queued event that names one, a line at
// a time, without the lock appends take, so no hook waits on it: an event appended
// meanwhile may be missed. It stops with ctx's error once ctx is done.
func (s *Spool) Sessions(ctx context.Context, add func(sessionID string)) error {
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, MaxBytes)
	for lines.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var e struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(lines.Bytes(), &e) == nil && e.SessionID != "" {
			add(e.SessionID)
		}
	}
	return lines.Err()
}

// Peek returns up to n queued events without consuming them.
func (s *Spool) Peek(n int) ([]Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil, err
	}
	defer unlock()
	events, err := s.readBatch(n, 0, time.Time{})
	return events.Events, err
}
