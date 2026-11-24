// Package spool is the local, append-only event queue every hook writes to: a hook
// appends one JSON line under the state directory and exits, and `terma spool flush`
// delivers later, so no hook ever waits on the backend.
package spool

import (
	"bufio"
	"bytes"
	"cmp"
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
	"github.com/miradorlabs/terma-cli/internal/sendas"
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
	// Account is the account key (auth.AccountKey) of the sign-in the event was collected
	// under, so it leaves under that sign-in only; "" when none was. An event with neither
	// it nor NoPerson is never queued, and one found queued is dropped. Local only.
	Account string `json:"account,omitempty"`
	// NoPerson marks an event collected under a server key: no person's, so its empty
	// Account is final, never bound to a sign-in later (AppendAsCollected). Local only.
	NoPerson    bool           `json:"no_person,omitempty"`
	Profile     string         `json:"profile,omitempty"`      // profile whose routing state routed the event; local only
	Enrollment  string         `json:"enrollment,omitempty"`   // destination enrollment sent to or holding the event; local only
	HoldKey     string         `json:"hold_key,omitempty"`     // pending binding holding the event until bound to the team's org; local only
	Run         string         `json:"run,omitempty"`          // session run the event belongs to (internal/runs); local only
	OwnLocation bool           `json:"own_location,omitempty"` // event's own checkout/dir; still dropped on Mixed unless runs.Locational
	Attrs       map[string]any `json:"attrs,omitempty"`
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

// EventsFile is the queue, in Dir. Its name, and every other file's this version keeps
// there, are new: an older terma's hook or flusher still running reads and writes only
// its own (events.jsonl), so it never sends what this version queued with its own
// credential, nor holds up or throttles this version's flushes. This version never reads
// an older terma's.
const EventsFile = "events-v2.jsonl"

const (
	backoffFile      = "next-attempt-v2"
	lastFlushFile    = "last-flush-v2"
	lockFile         = "flush-v2.lock"    // shared by appends and queue rewrites
	deliveryLockFile = "delivery-v2.lock" // held only by flushers
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
	// lane is whose what this Spool appends without a tag is (Bind, BindNoPerson): with
	// none, it is lost.
	lane Lane
}

// Bind has every event this Spool appends without an account carry account, as
// auth.Account names the sign-in collecting it. Signed out ("") or unreadable
// (sendas.AccountUnreadable), it names no one: what it would carry is lost.
func (s *Spool) Bind(account string) { s.lane = LaneOf(account, false) }

// BindNoPerson has every event this Spool appends without an account marked no person's
// (Event.NoPerson): collected under a server key, whichever path appends it.
func (s *Spool) BindNoPerson() { s.lane = LaneOf("", true) }

// Lane is whose what this Spool appends without a tag is. Unbound, it is no one's, as
// though the sign-in could not be read.
func (s *Spool) Lane() Lane { return s.lane }

// Open addresses (and creates) the spool directory. Until it is bound, what it appends
// without a tag is lost, counted as unreadable.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	return &Spool{dir: dir, lane: Lane{Lost: sendas.LostUnreadable}}, nil
}

func (s *Spool) path() string { return filepath.Join(s.dir, EventsFile) }

// Appendable reports whether the spool's queue can be opened to append to, which an agent's
// sandbox refuses. Unlike Writable it takes no lock and writes nothing, so a hook can ask
// at no cost while a flush holds the spool.
func (s *Spool) Appendable() bool {
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Append records events in one write under the lock Flush holds while rewriting the file,
// so they never land on a file about to be replaced.
func (s *Spool) Append(events ...Event) error {
	return s.AppendContext(context.Background(), events...)
}

// AppendContext observes caller cancellation and caps lock waits even when the
// caller has no deadline. An error means no event was queued.
func (s *Spool) AppendContext(ctx context.Context, events ...Event) error {
	return s.appendEvents(ctx, true, events)
}

// AppendAsCollected is Append for events that already carry the account they were
// collected under: a checkpointed event appended again later keeps it, whoever the Spool
// is bound to by then. One that carries none is dropped, uncounted: this version records
// none, so no one collected it under a sign-in that could send it.
func (s *Spool) AppendAsCollected(events ...Event) error {
	return s.appendEvents(context.Background(), false, events)
}

// appendEvents never queues an event that names neither an account nor no person: no one
// could ever send it. It is lost instead, counted by why its bound lane has none (Lost),
// and the rest are appended.
func (s *Spool) appendEvents(ctx context.Context, bind bool, events []Event) error {
	var lines []byte
	lost := map[string]int{}
	for _, e := range events {
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		if bind && e.Account == "" && !e.NoPerson {
			e.Account, e.NoPerson = s.lane.Account, s.lane.NoPerson
		}
		if !tagged(e) {
			if bind {
				lost[cmp.Or(s.lane.Lost, sendas.LostUnreadable)]++
			}
			continue
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		lines = append(append(lines, line...), '\n')
	}
	// Counted once the rest is queued: a failed append is retried, and would count them again.
	// A count that can't be written is no reason to fail the append.
	countLost := func() {
		for why, n := range lost {
			_ = s.CountLost(why, n)
		}
	}
	if len(lines) == 0 {
		countLost()
		return nil
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
	if _, err = f.Write(lines); err != nil {
		return err
	}
	countLost()
	return nil
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

// Sessions calls add with each queued event's session ID without locking. On non-nil error
// (partial results), callers must skip runs.Prune so queued sessions' ledgers stay intact.
func (s *Spool) Sessions(ctx context.Context, add func(sessionID string)) error {
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
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
