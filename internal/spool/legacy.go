package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// An older terma (v0.3.x) queues to LegacyEventsFile, under the lock legacyLockFile, and
// flushes it holding legacyDeliveryLockFile. Its lines name no account.
const (
	LegacyEventsFile       = "events.jsonl"
	legacyLockFile         = "flush.lock"
	legacyDeliveryLockFile = "delivery.lock"
)

// HasLegacy reports whether an older terma left anything queued in the spool directory
// dir: in its queue, or set aside by a decision that was never recorded (StageLegacy).
func HasLegacy(dir string) bool {
	for _, name := range []string{LegacyEventsFile, legacyStagedFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// AppendLegacy queues lines, whole JSON lines an older terma wrote, where it queues them
// (LegacyEventsFile), under its append lock: they are its backlog, taken in by IngestLegacy
// under the one decision of whose it is.
func (s *Spool) AppendLegacy(lines []byte) error {
	if len(lines) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, legacyLockFile))
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, LegacyEventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(lines)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// legacyStagedFile, in Dir, is what an older terma had queued at the decision of whose it is
// (StageLegacy), until IngestStaged takes it in under that decision. No older terma reads it.
const legacyStagedFile = "legacy-decided-v2.jsonl"

// StageLegacy sets what an older terma has queued aside, by one rename under its locks, so
// no append of its lands between: what it queues after goes to a new file. With something
// set aside already, by a run that stopped before recording the decision or taking it in,
// it does nothing.
func (s *Spool) StageLegacy() error {
	if _, err := os.Stat(filepath.Join(s.dir, legacyStagedFile)); err == nil {
		return nil
	}
	unlock, err := s.lockLegacy()
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Rename(filepath.Join(s.dir, LegacyEventsFile), filepath.Join(s.dir, legacyStagedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// IngestStaged is IngestLegacy for what StageLegacy set aside.
func (s *Spool) IngestStaged(account string, noPerson bool) error {
	return s.ingest(legacyStagedFile, account, noPerson)
}

// IngestLegacy moves what an older terma queued into this queue, tagged by its caller's
// decision of whose it is: account, or no person's (noPerson), or neither, when no one
// sign-in may send it, so it never leaves and ages out. A line that names an account or no
// person already keeps it. An older terma's hook still running keeps queueing, so it runs
// at every flush: each line is taken once.
//
// It holds the older terma's flush lock, so no older flusher is sending a line it moves,
// and both queues' locks, so no append lands on either between the read and the rewrite.
// A line not yet whole stays for its writer. A crash between the two writes leaves the
// moved lines in both, to be taken again: sent twice at most, never lost.
func (s *Spool) IngestLegacy(account string, noPerson bool) error {
	return s.ingest(LegacyEventsFile, account, noPerson)
}

// lockLegacy takes the older terma's flush and append locks, as it does to rewrite its queue.
func (s *Spool) lockLegacy() (unlock func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	var unlocks []func()
	unlock = func() {
		for _, u := range slices.Backward(unlocks) {
			u()
		}
	}
	for _, name := range []string{legacyDeliveryLockFile, legacyLockFile} {
		u, err := flock.Lock(ctx, filepath.Join(s.dir, name))
		if err != nil {
			unlock()
			return nil, err
		}
		unlocks = append(unlocks, u)
	}
	return unlock, nil
}

func (s *Spool) ingest(name, account string, noPerson bool) error {
	old := filepath.Join(s.dir, name)
	if _, err := os.Stat(old); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	unlockLegacy, err := s.lockLegacy()
	if err != nil {
		return err
	}
	defer unlockLegacy()
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	unlock, err := flock.Lock(ctx, filepath.Join(s.dir, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(old)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	whole := bytes.LastIndexByte(data, '\n') + 1
	if whole == 0 {
		return nil
	}
	var moved []byte
	for line := range bytes.Lines(data[:whole]) {
		var e Event
		if json.Unmarshal(line, &e) == nil {
			if e.Account == "" && !e.NoPerson {
				e.Account, e.NoPerson = account, noPerson
			}
			if b, err := json.Marshal(e); err == nil {
				line = append(b, '\n')
			}
		}
		// A line that doesn't decode moves as it is, and is counted dropped where it lands.
		moved = append(moved, line...)
	}
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(moved)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if whole == len(data) {
		return os.Remove(old)
	}
	return config.WriteFileAtomicNoSync(old, data[whole:], fileMode)
}
