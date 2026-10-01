// Package spool is the local, append-only event queue every hook writes to: a hook
// appends one JSON line under the config dir and exits, and `terma spool flush`
// delivers later, so no hook ever waits on the backend.
package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	Repo      string    `json:"repo,omitempty"`
	// Workspace is local policy context; the OTLP encoder never exports it.
	Workspace string `json:"workspace,omitempty"`
	// Global records machine-wide capture, so queued events can be withheld after a
	// switch back to repository coverage. Local only.
	Global bool           `json:"global,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
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

const (
	eventsFile       = "events.jsonl"
	backoffFile      = "next_attempt"
	lastFlushFile    = "last_flush"
	lockFile         = "flush.lock"    // shared by appends and queue rewrites
	deliveryLockFile = "delivery.lock" // held only by flushers
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

// Append records one event under the lock Flush holds while rewriting the file, so it
// never lands on a file about to be replaced.
func (s *Spool) Append(e Event) error {
	return s.AppendContext(context.Background(), e)
}

// AppendContext observes caller cancellation and caps lock waits even when the
// caller has no deadline. An error means the event was not queued.
func (s *Spool) AppendContext(ctx context.Context, e Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
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
	_, err = f.Write(line)
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

// NextAttempt is when the spool-wide backoff ends; zero when none is in force.
func (s *Spool) NextAttempt() time.Time {
	next, _ := s.backoff()
	return next
}

// FlushOptions tunes one flush.
type FlushOptions struct {
	// Timeout bounds the entire pass (DefaultFlushTimeout when nonpositive).
	Timeout time.Duration
	// Batch is events per send (DefaultBatch when zero).
	Batch int
	// Force ignores the failure backoff.
	Force bool
	// MinInterval skips the flush when one completed more recently (`terma spool flush --min-interval`).
	MinInterval time.Duration
	Now         time.Time
}

// SkipReason says why a flush did nothing without failing.
type SkipReason string

const (
	// SkipBackoff means a previous delivery failed and its backoff window is open.
	SkipBackoff SkipReason = "backing off after a failed delivery"
	// SkipMinInterval means a flush completed more recently than the caller's throttle.
	SkipMinInterval SkipReason = "a flush completed within the throttle interval"
)

// Result summarizes a flush; its counts are disjoint, and only Expired, Pruned and
// Dropped are loss.
type Result struct {
	Sent int
	// Held counts events handed back for later; still queued.
	Held int
	// Failed counts events a PartialDelivery left undelivered; still queued.
	Failed int
	// Expired counts events older than MaxAge.
	Expired int
	// Pruned counts events discarded to keep the spool under MaxBytes.
	Pruned int
	// Dropped counts lines that would not decode, such as a killed hook's torn write.
	Dropped int
	Skipped bool
	Reason  SkipReason
	Err     error
}

// add folds one read's losses into the pass; Sent and Held are counted at delivery.
func (r *Result) add(b batch) {
	r.Expired += b.Expired
	r.Pruned += b.Pruned
	r.Dropped += b.Dropped
}

// Flush delivers queued events, trimming what was sent; a failed send opens a
// spool-wide backoff (30s..1h), and a PartialDelivery is acknowledged and the pass continues.
func (s *Spool) Flush(ctx context.Context, sender Sender, opts FlushOptions) Result {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Only flushers take this lock, so hooks can append during a send.
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return Result{Err: err}
	}
	defer unlock()

	// Check backoff after the lock: the previous flusher may have failed meanwhile.
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if !opts.Force {
		if next := s.NextAttempt(); !next.IsZero() && now.Before(next) {
			return Result{Skipped: true, Reason: SkipBackoff}
		}
	}
	if opts.MinInterval > 0 {
		if last := s.LastFlush(); !last.IsZero() && now.Sub(last) < opts.MinInterval {
			return Result{Skipped: true, Reason: SkipMinInterval}
		}
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}

	var res Result
	// Partial failures end the pass as a failure without opening the spool-wide window.
	var failures []error
	stop := func(err error) Result {
		res.Err = errors.Join(append(failures, err)...)
		return res
	}
	// Consume only the bytes present at the start; requeued and new events wait for the next pass.
	budget := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return stop(err)
		}
		batchResult, err := s.readFlushBatch(ctx, batch, budget, now)
		res.add(batchResult)
		if err != nil {
			return stop(err)
		}
		if budget < 0 {
			budget = batchResult.Size
		}
		var held, undelivered []Event
		if len(batchResult.Events) > 0 {
			held, err = sender.Send(ctx, batchResult.Events)
			if partial, ok := errors.AsType[*PartialDelivery](err); ok {
				undelivered = partial.Undelivered
				if partial.Err != nil {
					failures = append(failures, partial)
				}
			} else if err != nil {
				failureTime := opts.Now
				if failureTime.IsZero() {
					failureTime = time.Now()
				}
				s.recordFailure(failureTime)
				res.Err = errors.Join(append(failures, err)...)
				return res
			}
		}
		// One atomic rewrite acknowledges the batch and requeues; if cancelled, the batch replays.
		requeue := slices.Concat(held, undelivered)
		if batchResult.Consumed > 0 {
			if err := s.ackBatch(ctx, batchResult.Consumed, batchResult.Prefix, requeue); err != nil {
				return stop(err)
			}
		}
		res.Sent += len(batchResult.Events) - len(requeue)
		res.Held += len(held)
		res.Failed += len(undelivered)
		budget -= batchResult.Consumed
		if len(batchResult.Events) < batch || budget <= 0 {
			// The pass reached every destination, so a spool-wide window has nothing to protect.
			s.clearBackoff()
			if len(failures) > 0 {
				res.Err = errors.Join(failures...)
				return res
			}
			s.recordFlush(now)
			return res
		}
	}
}

// batch is one read from the head of the queue, with every removed event counted once.
type batch struct {
	Events []Event
	// Consumed is the bytes an acknowledgement removes, skipped lines included.
	Consumed int64
	// Size is the file size the read saw, bounding what this pass may send.
	Size int64
	// Prefix hashes the consumed bytes, so the acknowledgement can refuse a changed queue.
	Prefix  [32]byte
	Expired int
	Pruned  int
	Dropped int
}

// readFlushBatch holds the append lock only while reading; it prunes only before
// the pass's initial byte budget.
func (s *Spool) readFlushBatch(ctx context.Context, n int, budget int64, now time.Time) (batch, error) {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return batch{}, err
	}
	defer unlock()
	pruned := 0
	if budget < 0 {
		if pruned, err = s.prune(); err != nil {
			return batch{}, err
		}
	}
	b, err := s.readBatch(n, max(budget, 0), now)
	b.Pruned = pruned // readBatch counts what it read, not what pruning removed
	return b, err
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

// readBatch decodes up to n events from the head, no further than a positive limit;
// undecodable and (when now is set) expired lines are counted and leave with the batch.
func (s *Spool) readBatch(n int, limit int64, now time.Time) (batch, error) {
	var b batch
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return batch{}, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return batch{}, err
	}
	b.Size = int64(len(data))
	for len(b.Events) < n {
		if limit > 0 && b.Consumed >= limit {
			break // the rest was appended after this pass began
		}
		idx := bytes.IndexByte(data[b.Consumed:], '\n')
		if idx < 0 || (limit > 0 && b.Consumed+int64(idx)+1 > limit) {
			break // leave an incomplete line (including one completed after the snapshot)
		}
		line := data[b.Consumed : b.Consumed+int64(idx)]
		b.Consumed += int64(idx) + 1
		var e Event
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &e); err != nil {
			b.Dropped++
			continue
		}
		if !now.IsZero() && !e.Time.IsZero() && now.Sub(e.Time) > MaxAge {
			b.Expired++
			continue
		}
		b.Events = append(b.Events, e)
	}
	b.Prefix = sha256.Sum256(data[:b.Consumed])
	return b, nil
}

// ackBatch removes a delivered prefix and requeues held events in one rename,
// keeping whatever was appended meanwhile.
func (s *Spool) ackBatch(ctx context.Context, consumed int64, prefix [32]byte, held []Event) error {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(s.path())
	if err != nil {
		return err
	}
	// A flusher that does not take delivery.lock may have trimmed this prefix meanwhile.
	if consumed > int64(len(data)) || sha256.Sum256(data[:consumed]) != prefix {
		return errors.New("spool changed during delivery")
	}
	tail := bytes.Clone(data[consumed:])
	for _, e := range held {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		tail = append(tail, line...)
		tail = append(tail, '\n')
	}
	return config.WriteFileAtomicNoSync(s.path(), tail, fileMode)
}

// prune bounds disk usage before a flush snapshot. Peek never mutates the queue:
// changing its prefix while a send is in flight would invalidate the acknowledgement.
func (s *Spool) prune() (int, error) {
	info, err := os.Stat(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Size() <= MaxBytes {
		return 0, nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		return 0, err
	}
	cut := len(data) - MaxBytes/2
	if idx := bytes.IndexByte(data[cut:], '\n'); idx >= 0 {
		cut += idx + 1
	} else {
		cut = len(data)
	}
	if err := config.WriteFileAtomicNoSync(s.path(), data[cut:], fileMode); err != nil {
		return 0, err
	}
	return bytes.Count(data[:cut], []byte{'\n'}), nil
}

// recordFailure doubles the previous wait (30s..1h); the file holds
// "<next-unix> <wait-seconds>" so the window survives between processes.
func (s *Spool) recordFailure(now time.Time) {
	wait := minBackoff
	if _, prev := s.backoff(); prev > 0 {
		wait = prev * 2
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	_ = os.WriteFile(filepath.Join(s.dir, backoffFile),
		[]byte(strconv.FormatInt(now.Add(wait).Unix(), 10)+" "+strconv.FormatInt(int64(wait/time.Second), 10)),
		fileMode)
}

func (s *Spool) backoff() (next time.Time, wait time.Duration) {
	data, err := os.ReadFile(filepath.Join(s.dir, backoffFile))
	if err != nil {
		return time.Time{}, 0
	}
	fields := bytes.Fields(data)
	if len(fields) == 0 {
		return time.Time{}, 0
	}
	unix, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return time.Time{}, 0
	}
	next = time.Unix(unix, 0)
	if len(fields) > 1 {
		if secs, err := strconv.ParseInt(string(fields[1]), 10, 64); err == nil {
			wait = time.Duration(secs) * time.Second
		}
	}
	return next, wait
}

func (s *Spool) clearBackoff() {
	_ = os.Remove(filepath.Join(s.dir, backoffFile))
}

// LastFlush is when a flush last ran to completion, or zero.
func (s *Spool) LastFlush() time.Time {
	data, err := os.ReadFile(filepath.Join(s.dir, lastFlushFile))
	if err != nil {
		return time.Time{}
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func (s *Spool) recordFlush(now time.Time) {
	_ = os.WriteFile(filepath.Join(s.dir, lastFlushFile), []byte(strconv.FormatInt(now.Unix(), 10)), fileMode)
}
