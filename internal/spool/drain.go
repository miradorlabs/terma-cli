package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// A Drain is a pass for a sender whose sends are slow and few, the image lane's: it settles
// each event on its own as it goes (Settle) rather than acknowledging a batch, so a pass cut
// short, cancelled or killed resends nothing it settled, and what it leaves unsettled keeps
// its place in the queue.

// Pending is a queued event as Drain hands it out, with the line it was read from, so it
// can be settled on its own.
type Pending struct {
	Event
	line []byte
}

// DrainOptions tunes one Drain.
type DrainOptions struct {
	// Timeout bounds the entire pass (DefaultFlushTimeout when nonpositive).
	Timeout time.Duration
	// NoWait skips the pass (SkipBusy) when another flusher is delivering the spool, rather
	// than waiting for it to finish.
	NoWait bool
	Now    time.Time
}

// errNotQueued is a settled event whose line the queue no longer holds.
var errNotQueued = errors.New("the event is no longer queued")

// Drain hands fn every event queued when the pass begins, once the queue is pruned to
// MaxBytes and rid of expired and unreadable lines, and holds the delivery lock until fn
// returns. fn settles what it is done with (Settle); the rest stays where it is. The
// result counts only what Drain itself let go.
func (s *Spool) Drain(ctx context.Context, opts DrainOptions, fn func(ctx context.Context, events []Pending)) Result {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var unlock func()
	var err error
	if opts.NoWait {
		unlock, err = flock.TryLock(filepath.Join(s.dir, deliveryLockFile))
		if flock.IsBusy(err) {
			return Result{Skipped: true, Reason: SkipBusy}
		}
	} else {
		unlock, err = flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	}
	if err != nil {
		return Result{Err: err}
	}
	defer unlock()
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	events, res, err := s.readPending(ctx, now)
	if err != nil {
		res.Err = err
		return res
	}
	if len(events) > 0 {
		fn(ctx, events)
	}
	return res
}

// readPending reads every whole line queued, under the append lock, and rewrites the queue
// without the lines it lets go: past MaxBytes, expired, or unreadable.
func (s *Spool) readPending(ctx context.Context, now time.Time) ([]Pending, Result, error) {
	var res Result
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil, res, err
	}
	defer unlock()
	if res.Pruned, err = s.prune(); err != nil {
		return nil, res, err
	}
	data, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, res, nil
	}
	if err != nil {
		return nil, res, err
	}
	var events []Pending
	var kept []byte
	rest := data
	for {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			break // a line still being written stays as it is
		}
		line := rest[:idx+1]
		rest = rest[idx+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			res.Dropped++
			continue
		}
		if !e.Time.IsZero() && now.Sub(e.Time) > MaxAge {
			res.Expired++
			continue
		}
		events = append(events, Pending{Event: e, line: line})
		kept = append(kept, line...)
	}
	if res.Dropped > 0 || res.Expired > 0 {
		if err := config.WriteFileAtomicNoSync(s.path(), append(kept, rest...), fileMode); err != nil {
			return nil, res, err
		}
	}
	return events, res, nil
}

// Settle takes p out of the queue and appends requeue at its tail, in one rewrite under the
// append lock. It waits for the lock at most ackTimeout whatever the pass's context, so a
// pass cut short still settles what it delivered. An event the queue no longer holds is an
// error, and requeues nothing.
func (s *Spool) Settle(p Pending, requeue ...Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(s.path())
	if err != nil {
		return err
	}
	at := lineAt(data, p.line)
	if at < 0 {
		return errNotQueued
	}
	out := append(bytes.Clone(data[:at]), data[at+len(p.line):]...)
	for _, e := range requeue {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out = append(append(out, line...), '\n')
	}
	return config.WriteFileAtomicNoSync(s.path(), out, fileMode)
}

// lineAt is where line, a whole line with its newline, first starts a line of data; -1
// when it is not there.
func lineAt(data, line []byte) int {
	for from := 0; from < len(data); {
		i := bytes.Index(data[from:], line)
		if i < 0 {
			return -1
		}
		at := from + i
		if at == 0 || data[at-1] == '\n' {
			return at
		}
		from = at + 1
	}
	return -1
}

// markFile holds the value a Drain's sender keeps between passes (Mark).
const markFile = "mark"

// Mark is the value a Drain's sender kept with SetMark, such as where its last pass
// began; "" when none was kept.
func (s *Spool) Mark() string {
	data, err := os.ReadFile(filepath.Join(s.dir, markFile))
	if err != nil {
		return ""
	}
	return string(data)
}

// SetMark keeps v for the next pass, best effort: only a Drain's sender, under the
// delivery lock, writes it.
func (s *Spool) SetMark(v string) {
	_ = config.WriteFileAtomicNoSync(filepath.Join(s.dir, markFile), []byte(v), fileMode)
}

// Idle waits until no flusher is delivering the spool, or ctx is done.
func (s *Spool) Idle(ctx context.Context) error {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return err
	}
	unlock()
	return nil
}
