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

// Queue is a spool sent one event at a time, slowly: the image lane's. Its sender reads
// the queue (Read), takes each event before it sends it (Take), so two senders never send
// one, and settles each as it goes (Taken.Settle), so a send cut short or a sender killed
// resends nothing settled. What it leaves keeps its place. Unlike a Spool it has no batch
// Flush, which would acknowledge lines out from under it.
//
// A settle does not rewrite the queue: it appends the line's hash to the settled log,
// synced, and a settled line counts as gone wherever the queue is read. Compact, at the end
// of a pass, and the next Read rewrite the queue once without what was settled.
//
// The log counts each settle, so of two lines alike, settling one leaves the other; but a
// crash between a compaction's rewrite and its removal of the log leaves hashes the next
// compaction applies again, dropping the twin of a line settled. Two lines alike are one
// record queued twice: the twin of one sent or let go adds nothing, so that is accepted.
type Queue struct {
	s *Spool
}

// OpenQueue addresses (and creates) the queue's directory.
func OpenQueue(dir string) (*Queue, error) {
	s, err := Open(dir)
	if err != nil {
		return nil, err
	}
	return &Queue{s: s}, nil
}

// settledFile is the settled log: one line hash, in hex, per settle.
const settledFile = "settled.log"

// Pending is a queued event as Read hands it out, with the line it was read from, so it
// can be taken and settled on its own.
type Pending struct {
	Event
	line []byte
	// read is the queue as Read saw it, which Take compares before it reads again.
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
func (q *Queue) Stamp() Stamp { return q.s.stamp() }

func (s *Spool) stamp() Stamp {
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

// Append queues events, as Spool.Append does.
func (q *Queue) Append(events ...Event) error { return q.s.Append(events...) }

// Read is every whole line queued, once the queue is compacted, pruned to MaxBytes and rid
// of expired and unreadable lines, which the result counts, and the queue's Stamp as it read
// it: a later Stamp that is not the Same says something was queued or settled since.
func (q *Queue) Read(ctx context.Context, now time.Time) ([]Pending, Stamp, Result, error) {
	s := q.s
	var res Result
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil, Stamp{}, res, err
	}
	defer unlock()
	if _, err := q.compact(Stamp{}); err != nil {
		return nil, Stamp{}, res, err
	}
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
			return nil, Stamp{}, res, err
		}
		stamp = s.stamp()
	}
	for i := range events {
		events[i].read = stamp
	}
	return events, stamp, res, nil
}

// Taken is an event a sender holds the delivery lock for: no other sender sends it until
// Release.
type Taken struct {
	q *Queue
	p Pending
	// release lets the delivery lock go.
	release func()
}

// Take takes the delivery lock for sending p, waiting for another sender until ctx ends.
// ErrNotQueued is p settled meanwhile, by whoever held the lock.
func (q *Queue) Take(ctx context.Context, p Pending) (*Taken, error) {
	unlock, err := flock.Lock(ctx, filepath.Join(q.s.dir, deliveryLockFile))
	if err != nil {
		return nil, err
	}
	// The log and the queue are read under the append lock every writer of either takes, so
	// they are read as one state.
	if err := q.underAppendLock(func() error { return q.checkQueued(p) }); err != nil {
		unlock()
		return nil, err
	}
	return &Taken{q: q, p: p, release: unlock}, nil
}

// checkQueued is nil when p is still queued: it compares the stamp Read saw, and reads the
// queue only when that changed or the log names p's line.
func (q *Queue) checkQueued(p Pending) error {
	settled, err := q.settled()
	if err == nil {
		switch {
		case settled[lineHash(p.line)] > 0:
			// Settled since Read, or (of two alike) its twin was: look whether one is left.
			err = q.stillQueued(p, settled)
		case !p.read.Same(q.Stamp()):
			// The queue changed since Read: an append keeps p, a rewrite may not.
			err = q.stillQueued(p, settled)
		}
	}
	return err
}

// underAppendLock runs fn holding the append lock, waiting ackTimeout for it at most.
func (q *Queue) underAppendLock(fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(q.s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// Release lets the event go to another sender.
func (t *Taken) Release() { t.release() }

// Settle takes the event out of the queue and appends requeue at its tail: Take found it
// queued, and the delivery lock kept every other sender from it since, so the queue is not
// read again.
func (t *Taken) Settle(requeue ...Event) error {
	tail, err := lines(requeue)
	if err != nil {
		return err
	}
	return t.q.settle([]Pending{t.p}, tail, false)
}

// stillQueued is nil when the queue holds more lines like p's than settled counts.
func (q *Queue) stillQueued(p Pending, settled map[string]int) error {
	data, err := os.ReadFile(q.s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotQueued
	}
	if err != nil {
		return err
	}
	if lineCount(data, p.line) <= settled[lineHash(p.line)] {
		return ErrNotQueued
	}
	return nil
}

// Settle takes p out of the queue and appends requeue at its tail, under the append lock,
// for a sender that did not Take it: it first checks p is still queued (ErrNotQueued, which
// requeues nothing). It waits for the lock at most ackTimeout whatever the sender's context,
// so a send cut short still settles what went.
func (q *Queue) Settle(p Pending, requeue ...Event) error {
	tail, err := lines(requeue)
	if err != nil {
		return err
	}
	return q.settle([]Pending{p}, tail, true)
}

// Remove takes every one of ps out of the queue, as Settle does, in one write of the
// settled log. ErrNotQueued removes none.
func (q *Queue) Remove(ps ...Pending) error {
	if len(ps) == 0 {
		return nil
	}
	return q.settle(ps, nil, true)
}

// lines is events encoded, a line each.
func lines(events []Event) ([]byte, error) {
	var out []byte
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out = append(append(out, line...), '\n')
	}
	return out, nil
}

// settle appends tail to the queue, then logs ps settled, synced, under the append lock;
// check first looks that each is still queued. The tail goes first, so a reader that sees
// a line settled (Mentions reads the log, then the queue) sees what replaced it too.
func (q *Queue) settle(ps []Pending, tail []byte, check bool) error {
	s := q.s
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	settled, err := q.settled()
	if err != nil {
		return err
	}
	var data []byte
	if check {
		if data, err = os.ReadFile(s.path()); err != nil {
			return err
		}
	}
	var log []byte
	for _, p := range ps {
		h := lineHash(p.line)
		if check && lineCount(data, p.line) <= settled[h] {
			return ErrNotQueued
		}
		settled[h]++
		log = append(append(log, h...), '\n')
	}
	if len(tail) > 0 {
		if err := appendSynced(s.path(), tail); err != nil {
			return err
		}
	}
	return appendSynced(filepath.Join(s.dir, settledFile), log)
}

// Mentions reports which of the needles appear in a queued line, settled lines not
// counted, as Spool.Mentions does. It reads the settled log before the queue: a settle
// appends its requeue before it logs, so a line seen settled has what replaced it queued.
func (q *Queue) Mentions(needles []string) (map[string]bool, error) {
	data, err := q.queued()
	if err != nil {
		return nil, err
	}
	return mentions(data, needles), nil
}

// Pending reports how many events are queued and the queue's size, settled lines not counted.
func (q *Queue) Pending() (count int, size int64, err error) {
	data, err := q.queued()
	if err != nil {
		return 0, 0, err
	}
	return bytes.Count(data, []byte{'\n'}), int64(len(data)), nil
}

// Peek returns up to n queued events without consuming them, once the queue is compacted.
func (q *Queue) Peek(n int) ([]Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(q.s.dir, lockFile))
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := q.compact(Stamp{}); err != nil {
		return nil, err
	}
	events, err := q.s.readBatch(n, 0, time.Time{})
	return events.Events, err
}

// queued is the queue's lines without those settled since the last compaction; nil with
// no queue. The log is read first (see Mentions).
func (q *Queue) queued() ([]byte, error) {
	settled, err := q.settled()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(q.s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || len(settled) == 0 {
		return data, err
	}
	return withoutSettled(data, settled), nil
}

// Idle waits until no sender holds an event of the queue, or ctx is done.
func (q *Queue) Idle(ctx context.Context) error {
	unlock, err := flock.Lock(ctx, filepath.Join(q.s.dir, deliveryLockFile))
	if err != nil {
		return err
	}
	unlock()
	return nil
}
