// Package spool is the local, append-only event queue every hook writes to.
//
// The rule that decides whether developers tolerate the tool: no hook ever blocks
// on the backend being reachable. A hook appends one line to a file and exits;
// delivery happens later, from `terma spool flush`, which hooks kick off detached
// and which doctor/status run inline. A collector having a bad day costs nothing at
// commit time.
//
// The file is JSON Lines under the user's config dir (events never enter a
// repository). A short file lock protects appends and atomic rewrites. A separate
// delivery lock serializes flushes; network requests never hold the file lock.
// Appends that land during a send are kept for the next pass.
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
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Event is one thing that happened on this machine worth telling the backend.
type Event struct {
	Time      time.Time      `json:"time"`
	Name      string         `json:"name"`
	SessionID string         `json:"session_id,omitempty"`
	Repo      string         `json:"repo,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

// Sender delivers a batch. It must be all-or-nothing per call: on error the batch
// stays spooled and is retried later. Send must respect context cancellation.
type Sender interface {
	// Send delivers a batch. Events it cannot deliver yet but should not lose —
	// typically ones whose project has no key on this machine until `terma install`
	// runs — are returned as held; they go back to the end of the queue. An error
	// means nothing was accepted and the whole batch stays put.
	Send(ctx context.Context, events []Event) (held []Event, err error)
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, events []Event) ([]Event, error)

// Send calls f.
func (f SenderFunc) Send(ctx context.Context, events []Event) ([]Event, error) {
	return f(ctx, events)
}

const (
	eventsFile  = "events.jsonl"
	backoffFile = "next_attempt"
	// lastFlushFile records when a flush last ran to completion, so that hooks
	// firing every turn can ask for a flush "unless one ran recently".
	lastFlushFile    = "last_flush"
	lockFile         = "flush.lock"    // shared by appends and queue rewrites
	deliveryLockFile = "delivery.lock" // held only by flushers
	// probeEventName marks the line Writable writes and immediately truncates.
	// Nothing else may use it: it is the one name that never reaches a sender.
	probeEventName = "terma-write-probe"
	// MaxBytes bounds the spool on a machine that can never reach the backend. When
	// exceeded, the oldest events are dropped at the next flush — dropping
	// telemetry beats filling a disk.
	MaxBytes = 16 << 20
	// MaxAge bounds how long an undelivered event is worth delivering. A backend
	// unreachable for two weeks has lost the session anyway, and a queue that
	// only ever grows is the kind of footprint a developer notices. Older events
	// are dropped at the next flush and counted.
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

// Append records one event. This is the hook fast path: one encode, one locked
// append. The lock is the same one Flush holds while it rewrites the file, so an
// append can never land on a file that is about to be replaced and vanish.
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
	// The budget covers the lock wait, so it starts here and not before the
	// encode: serializing a large event on a loaded machine can take longer than
	// the lock may wait, and charging that to the wait would refuse an unheld
	// lock — losing the event while blaming a peer that never had it.
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

// Writable reports whether an event would actually land. Nothing else answers the
// question: Pending reads the queue, so a spool that exists but cannot be written —
// a read-only mount, a full disk, a config dir whose ownership changed under a
// running install — reports an empty, healthy queue while every hook silently drops
// what it records.
//
// It writes a probe line at the end of the queue and truncates it back off, all
// under the append lock, so no concurrent append or flush can observe the probe and
// nothing but the probe's own bytes is ever removed. The syncs are what make this
// worth doing at all: a full disk can accept a write and only refuse it later.
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
	if err := f.Sync(); err != nil {
		_ = f.Truncate(info.Size()) // best effort: leave the queue as it was
		return err
	}
	if err := f.Truncate(info.Size()); err != nil {
		return err
	}
	return f.Sync()
}

// NextAttempt is when the flusher will try again after a failure; zero when no
// backoff is in force.
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
	// MinInterval skips the flush when one completed more recently than this: the
	// throttle behind `terma spool flush --min-interval`, for a caller that flushes on a
	// timer. Hooks do not set it. Zero means always flush.
	MinInterval time.Duration
	Now         time.Time
}

// SkipReason says why a flush did nothing without failing. It is typed rather
// than a bool because the two cases want different answers from whoever asked for
// the flush: a backoff window clears itself, and the min-interval throttle means
// "someone else has just delivered".
type SkipReason string

const (
	// SkipBackoff means a previous delivery failed and its backoff window is open.
	SkipBackoff SkipReason = "backing off after a failed delivery"
	// SkipMinInterval means a flush completed more recently than the caller's throttle.
	SkipMinInterval SkipReason = "a flush completed within the throttle interval"
)

// Result summarizes a flush. The counts are disjoint: every event the pass looked
// at either went out, is still queued, or is one of the three kinds of loss below.
type Result struct {
	// Sent counts events the sender accepted.
	Sent int
	// Held counts events the sender handed back for a later attempt. They are
	// still queued, not lost.
	Held int
	// Expired counts events given up on for age: older than MaxAge, held or not.
	// Time, not corruption.
	Expired int
	// Pruned counts events discarded to keep the spool under MaxBytes. Disk
	// pressure, not corruption.
	Pruned int
	// Dropped counts lines the flusher could not decode — a torn write from a
	// killed hook. This is the only counter that means the queue was unreadable.
	Dropped int
	// Skipped is set when the pass did nothing. Reason carries which case.
	Skipped bool
	Reason  SkipReason
	Err     error
}

// add folds one read's losses into the pass total. Sent and Held are counted at
// the point of delivery, not here, because only the sender knows which of a
// batch's events it accepted.
func (r *Result) add(b batch) {
	r.Expired += b.Expired
	r.Pruned += b.Pruned
	r.Dropped += b.Dropped
}

// Flush delivers queued events. Bytes sent are trimmed from the file under the
// lock; anything appended meanwhile survives. On a send failure the remaining
// events stay and an exponential backoff (30s .. 1h) is recorded so a dead backend
// is not hammered by every commit.
func (s *Spool) Flush(ctx context.Context, sender Sender, opts FlushOptions) Result {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Only flushers take this lock. Hooks remain free to append during a send.
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return Result{Err: err}
	}
	defer unlock()

	// Check backoff after acquiring the delivery lock: the previous flusher may
	// have failed while this one waited.
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
	// Consume only the bytes present at the start. Held events and concurrent
	// appends stay at the tail for the next pass.
	budget := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res
		}
		batchResult, err := s.readFlushBatch(ctx, batch, budget, now)
		res.add(batchResult)
		if err != nil {
			res.Err = err
			return res
		}
		if budget < 0 {
			budget = batchResult.Size
		}
		var held []Event
		if len(batchResult.Events) > 0 {
			held, err = sender.Send(ctx, batchResult.Events)
			if err != nil {
				failureTime := opts.Now
				if failureTime.IsZero() {
					failureTime = time.Now()
				}
				s.recordFailure(failureTime)
				res.Err = err
				return res
			}
		}
		// Acknowledging delivery is one atomic rewrite, including held events.
		// If cancellation prevents it, the original batch remains for replay.
		if batchResult.Consumed > 0 {
			if err := s.ackBatch(ctx, batchResult.Consumed, batchResult.Prefix, held); err != nil {
				res.Err = err
				return res
			}
		}
		res.Sent += len(batchResult.Events) - len(held)
		res.Held += len(held)
		budget -= batchResult.Consumed
		if len(batchResult.Events) < batch || budget <= 0 {
			s.clearBackoff()
			s.recordFlush(now)
			return res
		}
	}
}

// batch is one read from the head of the queue, with every event it removed
// accounted for exactly once. Prune loss and decode loss stay apart because they
// mean different things: one is the disk bound doing its job, the other is a
// line the flusher could not read.
type batch struct {
	// Events are the decodable events at the head, in queue order.
	Events []Event
	// Consumed is how many bytes Events (plus everything skipped over with them)
	// occupied, which is what an acknowledgement removes.
	Consumed int64
	// Size is the file size the read saw, bounding what this pass may send.
	Size int64
	// Prefix hashes the consumed bytes, so the acknowledgement can refuse to trim
	// a queue that changed underneath it.
	Prefix [32]byte
	// Expired counts events past MaxAge.
	Expired int
	// Pruned counts events the disk bound discarded before this read.
	Pruned int
	// Dropped counts lines that would not decode — a torn write from a killed hook.
	Dropped int
}

// readFlushBatch holds the append lock only while reading/pruning local data.
// Only a flusher can prune, and only before taking its initial byte budget.
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

// readBatch decodes up to n events from the head of the file (and, when limit is
// positive, from no further than limit bytes in). Undecodable lines (a torn write
// from a killed hook) and, when now is set, events older than MaxAge are skipped
// over and counted in the batch; they leave the file with the batch they were read
// in.
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

// ackBatch removes a delivered prefix and requeues held events in one rename.
// It reads the current tail under the append lock, preserving concurrent appends.
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
	// A pre-upgrade flusher does not know delivery.lock. If it acknowledged or
	// pruned this prefix during our send, never trim unrelated newer events.
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

// recordFailure schedules the next attempt at double the previous wait (30s at
// first, capped at an hour). The file holds "<next-unix> <wait-seconds>" so the
// window length survives between processes.
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

// backoff reads the scheduled next attempt and the wait that produced it.
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
