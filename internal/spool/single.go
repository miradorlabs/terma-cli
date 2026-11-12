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

// Events sent one at a time, slowly (the image lane's), go another way than Flush's
// batches: the sender reads the queue (Read), takes each event before it sends it (Take),
// so two senders never send one, and settles each on its own as it goes (Settle), so a send
// cut short, or a sender killed, resends nothing settled. What it leaves keeps its place.

// Pending is a queued event as Read hands it out, with the line it was read from, so it
// can be taken and settled on its own.
type Pending struct {
	Event
	line []byte
}

// ErrNotQueued is an event whose line the queue no longer holds: another sender settled it.
var ErrNotQueued = errors.New("the event is no longer queued")

// Read is every whole line queued, once the queue is pruned to MaxBytes and rid of expired
// and unreadable lines, which the result counts.
func (s *Spool) Read(ctx context.Context, now time.Time) ([]Pending, Result, error) {
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

// Take takes the delivery lock for sending p, waiting for another sender until ctx ends,
// and returns its release. ErrNotQueued is p settled meanwhile, by whoever held the lock.
func (s *Spool) Take(ctx context.Context, p Pending) (release func(), err error) {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path())
	if err == nil && lineAt(data, p.line) < 0 || errors.Is(err, fs.ErrNotExist) {
		err = ErrNotQueued
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// Settle takes p out of the queue and appends requeue at its tail, in one rewrite under the
// append lock. It waits for the lock at most ackTimeout whatever the sender's context, so a
// send cut short still settles what went. ErrNotQueued requeues nothing.
func (s *Spool) Settle(p Pending, requeue ...Event) error {
	var tail []byte
	for _, e := range requeue {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		tail = append(append(tail, line...), '\n')
	}
	return s.rewrite([]Pending{p}, tail)
}

// Remove takes every one of ps out of the queue, in one rewrite as Settle does.
// ErrNotQueued removes none.
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
			return ErrNotQueued
		}
		data = append(bytes.Clone(data[:at]), data[at+len(p.line):]...)
	}
	return config.WriteFileAtomicNoSync(s.path(), append(data, tail...), fileMode)
}

// Size is the queue file's size in bytes, 0 when there is none: one stat, for a sender
// deciding whether anything was queued since it last looked.
func (s *Spool) Size() int64 {
	info, err := os.Stat(s.path())
	if err != nil {
		return 0
	}
	return info.Size()
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

// Idle waits until no flusher is delivering the spool, or ctx is done.
func (s *Spool) Idle(ctx context.Context) error {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return err
	}
	unlock()
	return nil
}
