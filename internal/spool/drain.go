package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
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
	pass *drainPass
}

// drainPass is what one Drain has handed out, or requeued, by line: a line queued more
// than once counts each time.
type drainPass struct {
	seen map[string]int
}

// fresh is the events of queued not handed out yet this pass, which it now counts as handed out.
func (d *drainPass) fresh(queued []Pending) []Pending {
	left := maps.Clone(d.seen)
	var out []Pending
	for _, p := range queued {
		if left[string(p.line)] > 0 {
			left[string(p.line)]--
			continue
		}
		p.pass = d
		d.seen[string(p.line)]++
		out = append(out, p)
	}
	return out
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

// Drain hands fn every event queued, once the queue is pruned to MaxBytes and rid of
// expired and unreadable lines, holding the delivery lock until it is done. fn settles what
// it is done with (Settle); the rest stays where it is. When fn returns with time left, any
// events appended meanwhile go to fn too, until none are new: an event fn requeued is not
// new. The result counts only what Drain itself let go.
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
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var res Result
	pass := &drainPass{seen: map[string]int{}}
	for ctx.Err() == nil {
		queued, read, err := s.readPending(ctx, now)
		res.Expired += read.Expired
		res.Pruned += read.Pruned
		res.Dropped += read.Dropped
		if err != nil {
			res.Err = err
			break
		}
		events := pass.fresh(queued)
		if len(events) == 0 {
			break
		}
		fn(ctx, events)
	}
	drainEnding()
	unlock()
	// An append between the last read and the unlock found the pass running and started none.
	res.Grew = pass.unseen(s)
	return res
}

// drainEnding runs as a Drain stops looking, before it unlocks: a test appends there.
var drainEnding = func() {}

// unseen reports whether the queue holds a whole line the pass never handed out nor
// requeued. It reads the file as Mentions does, without the lock.
func (d *drainPass) unseen(s *Spool) bool {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return false
	}
	left := maps.Clone(d.seen)
	for line := range bytes.Lines(data) {
		if !bytes.HasSuffix(line, []byte{'\n'}) || len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if left[string(line)] == 0 {
			return true
		}
		left[string(line)]--
	}
	return false
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
	var tail []byte
	for _, e := range requeue {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if p.pass != nil {
			// Requeued, it is not new to the pass that settled it.
			p.pass.seen[string(line)]++
		}
		tail = append(tail, line...)
	}
	return s.rewrite([]Pending{p}, tail)
}

// Remove takes every one of ps out of the queue, in one rewrite as Settle does. An event
// the queue no longer holds is an error, and removes none.
func (s *Spool) Remove(ps ...Pending) error {
	if len(ps) == 0 {
		return nil
	}
	return s.rewrite(ps, nil)
}

// rewrite takes ps's lines out of the queue and appends tail, under the append lock.
func (s *Spool) rewrite(ps []Pending, tail []byte) error {
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
	for _, p := range ps {
		at := lineAt(data, p.line)
		if at < 0 {
			return errNotQueued
		}
		data = append(bytes.Clone(data[:at]), data[at+len(p.line):]...)
	}
	return config.WriteFileAtomicNoSync(s.path(), append(data, tail...), fileMode)
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
