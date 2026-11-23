package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

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
// dir.
func HasLegacy(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, LegacyEventsFile))
	return err == nil && info.Size() > 0
}

// IngestLegacy moves what an older terma queued into this queue, tagged by its caller's
// decision of whose it is: account, or no person's (noPerson), or neither, when no one
// sign-in may send it, so it never leaves and ages out. A line that names an account or no
// person already keeps it. It runs whenever a decision is kept, since an older terma's
// hook still running keeps queueing: each line is taken once.
//
// It holds the older terma's flush lock, so no older flusher is sending a line it moves,
// and both queues' locks, so no append lands on either between the read and the rewrite.
// A line not yet whole stays for its writer. A crash between the two writes leaves the
// moved lines in both, to be taken again: sent twice at most, never lost.
func (s *Spool) IngestLegacy(account string, noPerson bool) error {
	old := filepath.Join(s.dir, LegacyEventsFile)
	if _, err := os.Stat(old); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), localLockTimeout)
	defer cancel()
	for _, name := range []string{legacyDeliveryLockFile, legacyLockFile, lockFile} {
		unlock, err := flock.Lock(ctx, filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		defer unlock()
	}
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
