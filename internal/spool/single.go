package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
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
//
// A settle does not rewrite the queue: it appends the line's hash to the settled log
// (settledFile), synced, and a settled line counts as gone wherever the queue is read. The
// next Read, or the sender's Compact at the end of its pass, rewrites the queue once without
// what was settled and empties the log. A crash between leaves the log, which the next Read
// compacts: a settled line is never handed out again. The log counts each settle, so of two
// lines alike, settling one leaves the other.

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

// Read is every whole line queued, once the queue is compacted (Compact), pruned to MaxBytes
// and rid of expired and unreadable lines, which the result counts, and the queue's Stamp as
// it read it: a later Stamp that is not the Same says something was queued or settled since.
func (s *Spool) Read(ctx context.Context, now time.Time) ([]Pending, Stamp, Result, error) {
	var res Result
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil, Stamp{}, res, err
	}
	defer unlock()
	if err := s.compact(); err != nil {
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
		stamp = s.Stamp()
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
	settled, err := s.settled()
	if err == nil {
		switch {
		case settled[lineHash(p.line)] > 0:
			// Settled since Read, or (of two alike) its twin was: look whether one is left.
			err = s.stillQueued(p, settled)
		case !p.read.Same(s.Stamp()):
			// The queue changed since Read: an append keeps p, a rewrite may not.
			err = s.stillQueued(p, settled)
		}
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// stillQueued is nil when the queue holds more lines like p's than settled counts.
func (s *Spool) stillQueued(p Pending, settled map[string]int) error {
	data, err := os.ReadFile(s.path())
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

// Settle takes p out of the queue, by the settled log, and appends requeue at its tail,
// under the append lock. It waits for the lock at most ackTimeout whatever the sender's
// context, so a send cut short still settles what went. ErrNotQueued requeues nothing.
func (s *Spool) Settle(p Pending, requeue ...Event) error {
	var tail []byte
	for _, e := range requeue {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		tail = append(append(tail, line...), '\n')
	}
	return s.settle([]Pending{p}, tail)
}

// Remove takes every one of ps out of the queue, as Settle does, in one write of the
// settled log. ErrNotQueued removes none.
func (s *Spool) Remove(ps ...Pending) error {
	if len(ps) == 0 {
		return nil
	}
	return s.settle(ps, nil)
}

// settle logs ps settled, synced, and appends tail to the queue, under the append lock.
func (s *Spool) settle(ps []Pending, tail []byte) error {
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
	settled, err := s.settled()
	if err != nil {
		return err
	}
	var log []byte
	for _, p := range ps {
		h := lineHash(p.line)
		if lineCount(data, p.line) <= settled[h] {
			return ErrNotQueued
		}
		settled[h]++
		log = append(append(log, h...), '\n')
	}
	if err := appendSynced(filepath.Join(s.dir, settledFile), log); err != nil {
		return err
	}
	if len(tail) > 0 {
		return appendSynced(s.path(), tail)
	}
	return nil
}

// Compact rewrites the queue without the lines settled since the last compaction, once,
// and empties the settled log: the sender's end of a pass.
func (s *Spool) Compact() error {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	return s.compact()
}

// compact is Compact, under the append lock. The log goes only once the queue without its
// lines is in place; a crash between leaves hashes of lines no longer queued, which match
// nothing and go at the next compaction.
func (s *Spool) compact() error {
	settled, err := s.settled()
	if err != nil || len(settled) == 0 {
		return err
	}
	data, err := os.ReadFile(s.path())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := config.WriteFileAtomicNoSync(s.path(), withoutSettled(data, settled), fileMode); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(s.dir, settledFile))
}

// settled is the settled log as a count of settles per line hash; empty with no log.
func (s *Spool) settled() (map[string]int, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, settledFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for line := range bytes.Lines(data) {
		if h := string(bytes.TrimSpace(line)); h != "" {
			counts[h]++
		}
	}
	return counts, nil
}

// withoutSettled is data without the first settled[hash] whole lines of each hash; a line
// still being written stays.
func withoutSettled(data []byte, settled map[string]int) []byte {
	left := maps.Clone(settled)
	var out []byte
	for line := range bytes.Lines(data) {
		if bytes.HasSuffix(line, []byte{'\n'}) {
			if h := lineHash(line); left[h] > 0 {
				left[h]--
				continue
			}
		}
		out = append(out, line...)
	}
	return out
}

// lineHash names a whole line, its newline included, in the settled log.
func lineHash(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// lineCount is how many whole lines of data are line.
func lineCount(data, line []byte) int {
	n := 0
	for l := range bytes.Lines(data) {
		if bytes.Equal(l, line) {
			n++
		}
	}
	return n
}

// appendSynced appends data to the file at path, and syncs it.
func appendSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
