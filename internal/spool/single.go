package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// at is where Read found the line in the queue, which Take looks at first; read is the
	// queue as Read saw it, which Take compares before it reads anything.
	at   int64
	read Stamp
}

// Stamp is one state of the queue file: its size, its modification time and the file
// itself, which every rewrite replaces (an atomic rename). An append changes the first two,
// a settle the third, so no change is missed, an equal-length append after a settle
// included. It is taken from an open file (stampOf): on Windows that alone records the
// file's identity as it was, where a stat by path leaves os.SameFile to look it up later,
// at whatever the path names by then.
type Stamp struct {
	size int64
	mod  time.Time
	file os.FileInfo
}

// Same reports whether a and b are one state of the queue: nothing appended or rewritten
// between them.
func (a Stamp) Same(b Stamp) bool {
	if a.file == nil || b.file == nil {
		return a.file == nil && b.file == nil
	}
	return a.size == b.size && a.mod.Equal(b.mod) && os.SameFile(a.file, b.file)
}

// Stamp is the queue as it stands now: one open and stat.
func (s *Spool) Stamp() Stamp {
	f, err := os.Open(s.path())
	if err != nil {
		return Stamp{}
	}
	defer f.Close()
	return stampOf(f)
}

// stampOf is the Stamp of the open file f.
func stampOf(f *os.File) Stamp {
	info, err := f.Stat()
	if err != nil {
		return Stamp{}
	}
	return Stamp{size: info.Size(), mod: info.ModTime(), file: info}
}

// ErrNotQueued is an event whose line the queue no longer holds: another sender settled it.
var ErrNotQueued = errors.New("the event is no longer queued")

// Read is every whole line queued, once the queue is pruned to MaxBytes and rid of expired
// and unreadable lines, which the result counts, and the queue's Stamp as it read it: a
// later Stamp that is not the Same says something was queued or settled since.
func (s *Spool) Read(ctx context.Context, now time.Time) ([]Pending, Stamp, Result, error) {
	var res Result
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil, Stamp{}, res, err
	}
	defer unlock()
	if res.Pruned, err = s.prune(); err != nil {
		return nil, Stamp{}, res, err
	}
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Stamp{}, res, nil
	}
	if err != nil {
		return nil, Stamp{}, res, err
	}
	// Taken under the lock every append and rewrite takes, from the file read: what was read.
	stamp := stampOf(f)
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return nil, Stamp{}, res, err
	}
	var events []Pending
	var kept []byte
	rest := data
	for {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			break // a line still being written stays as it is
		}
		at := int64(len(data) - len(rest))
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
		events = append(events, Pending{Event: e, line: line, at: at})
		kept = append(kept, line...)
	}
	if res.Dropped > 0 || res.Expired > 0 {
		if err := config.WriteFileAtomicNoSync(s.path(), append(kept, rest...), fileMode); err != nil {
			return nil, Stamp{}, res, err
		}
		stamp = s.Stamp()
		// The rewrite keeps the events in order, without what it dropped.
		var at int64
		for i := range events {
			events[i].at = at
			at += int64(len(events[i].line))
		}
	}
	for i := range events {
		events[i].read = stamp
	}
	return events, stamp, res, nil
}

// Take takes the delivery lock for sending p, waiting for another sender until ctx ends,
// and returns its release. ErrNotQueued is p settled meanwhile, by whoever held the lock.
func (s *Spool) Take(ctx context.Context, p Pending) (release func(), err error) {
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, deliveryLockFile))
	if err != nil {
		return nil, err
	}
	if p.read.file != nil && p.read.Same(s.Stamp()) || s.lineAtOffset(p) {
		// Nothing appended or rewritten since Read, or p's line is where Read found it: p is
		// still queued, without reading the queue.
		return unlock, nil
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

// lineAtOffset reports whether p's line still starts a line where Read found it.
func (s *Spool) lineAtOffset(p Pending) bool {
	f, err := os.Open(s.path())
	if err != nil {
		return false
	}
	defer f.Close()
	from := max(p.at-1, 0)
	buf := make([]byte, p.at-from+int64(len(p.line)))
	if _, err := f.ReadAt(buf, from); err != nil {
		return false
	}
	if p.at > 0 && buf[0] != '\n' {
		return false
	}
	return bytes.Equal(buf[p.at-from:], p.line)
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
