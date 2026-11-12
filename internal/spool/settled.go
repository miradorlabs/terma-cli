package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// A Queue's settled log, and the compaction that folds it into the queue (see Queue).

// Compact rewrites the queue without the lines settled since the last compaction, once,
// synced, and empties the settled log: the end of a sender's pass. It returns the queue's
// Stamp after when the queue was still as expect said before (nothing but settles since),
// so the compaction itself is no news to the sender; expect otherwise.
func (q *Queue) Compact(expect Stamp) (Stamp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(q.s.dir, lockFile))
	if err != nil {
		return expect, err
	}
	defer unlock()
	return q.compact(expect)
}

// compact is Compact, under the append lock. The log goes only once the queue without its
// lines is in place and synced.
func (q *Queue) compact(expect Stamp) (Stamp, error) {
	s := q.s
	unchanged := s.stamp().Same(expect)
	settled, err := q.settled()
	if err != nil {
		return expect, err
	}
	if len(settled) > 0 {
		data, err := os.ReadFile(s.path())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return expect, err
		}
		if err == nil {
			if err := config.WriteFileAtomic(s.path(), withoutSettled(data, settled), fileMode); err != nil {
				return expect, err
			}
		}
		if err := os.Remove(filepath.Join(s.dir, settledFile)); err != nil {
			return expect, err
		}
	}
	if !unchanged {
		return expect, nil
	}
	return s.stamp(), nil
}

// settled is the settled log as a count of settles per line hash; empty with no log.
func (q *Queue) settled() (map[string]int, error) {
	data, err := os.ReadFile(filepath.Join(q.s.dir, settledFile))
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
