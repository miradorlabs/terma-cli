package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A crossing a hook could not write to the ledger (its lock held past lockWait, or the write
// failed) is recorded beside it without the lock, in the session's sidecar: runs/<session>.mixed,
// a folder of marks, each a run and when it became Mixed, in a file of its own written
// atomically, so that hooks marking at once never lose one another's. It never expires. Every
// read of the ledger takes it for Mixed (Read, Locate, MixedAt, Mixed); the next edit under
// the ledger's lock folds it in and removes it (fold); Prune keeps it while it keeps the
// ledger, and one with no ledger for MixedRetention past its last mark.
//
// A read takes the sidecar before the ledger, and a fold saves the ledger before it removes
// what it folded: so a read that runs alongside a fold finds every mark in one or the other.

// asideExt ends a sidecar's name.
const asideExt = ".mixed"

// aside is a session's sidecar, as read.
type aside struct {
	marks []mark
	// files are the marks' file names, for fold to remove.
	files []string
}

// mark is a crossing recorded aside: its run, and when that run became Mixed.
type mark struct {
	Run string    `json:"run"`
	At  time.Time `json:"at"`
}

// asideWrites bounds MarkAside's attempts: a fold may remove the emptied folder between
// the attempt's making it and writing there.
const asideWrites = 3

// MarkAside records in sessionID's sidecar, without the ledger's lock, that its run runID
// became Mixed at at, for a crossing MarkMixed could not record. With no run ID (the ledger
// names none) it names a new run that covers every earlier record, as MarkMixed would start.
func MarkAside(stateDir, sessionID, runID string, at time.Time) error {
	dir, err := asideDir(stateDir, sessionID)
	if err != nil {
		return err
	}
	if runID == "" {
		if runID, err = newRunID(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(mark{Run: runID, At: at.UTC()})
	if err != nil {
		return err
	}
	name, err := newRunID()
	if err != nil {
		return err
	}
	for range asideWrites {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			if err = config.WriteFileAtomic(filepath.Join(dir, name+".json"), b, 0o600); err == nil {
				return nil
			}
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return err
}

// apply marks l's runs Mixed as a's marks say, an earlier time winning: each mark's run, or
// the current one when the ledger does not know it, as MarkMixed marks it; with no run
// recorded, one that covers every earlier record.
func (a aside) apply(l *ledger) {
	for _, m := range a.marks {
		i := slices.IndexFunc(l.Runs, func(r run) bool { return r.ID == m.Run })
		switch {
		case i >= 0:
		case len(l.Runs) == 0:
			l.Runs, i = []run{{ID: m.Run}}, 0
		default:
			i = len(l.Runs) - 1
		}
		if cur := &l.Runs[i]; cur.MixedAt.IsZero() || m.At.Before(cur.MixedAt) {
			cur.MixedAt = m.At
		}
		l.active(m.At)
	}
}

// last is a's latest mark's time; zero for none.
func (a aside) last() time.Time {
	var out time.Time
	for _, m := range a.marks {
		out = latest(out, m.At)
	}
	return out
}

// fold takes sessionID's sidecar into l, the ledger at path that the caller holds the lock
// of, saves it, and only then removes the marks it took: a mark written meanwhile is a file
// of its own, and stays for the next fold.
func fold(stateDir, sessionID, path string, l *ledger) error {
	dir, err := asideDir(stateDir, sessionID)
	if err != nil {
		return err
	}
	a, err := loadAside(dir)
	if err != nil || len(a.marks) == 0 {
		return err
	}
	a.apply(l)
	if err := save(path, l); err != nil {
		return err
	}
	for _, name := range a.files {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	_ = os.Remove(dir) // only once empty
	return nil
}

// readAside is sessionID's sidecar, for a read, which takes it before the ledger.
func readAside(stateDir, sessionID string) (aside, error) {
	dir, err := asideDir(stateDir, sessionID)
	if err != nil {
		return aside{}, nil
	}
	return loadAside(dir)
}

func asideDir(stateDir, sessionID string) (string, error) {
	if !session.ValidID(sessionID) {
		return "", fmt.Errorf("runs: invalid session id %q", sessionID)
	}
	return filepath.Join(stateDir, Dir, sessionID+asideExt), nil
}

// loadAside reads the marks in the sidecar dir, skipping a write under way and a mark a
// fold removed since the folder was listed, which that fold saved in the ledger first. One
// that cannot be read is an error, as an unreadable ledger is.
func loadAside(dir string) (aside, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return aside{}, nil
	}
	if err != nil {
		return aside{}, err
	}
	var a aside
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), config.TempPrefix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return aside{}, err
		}
		var m mark
		if err := json.Unmarshal(b, &m); err != nil {
			return aside{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, e.Name()), err)
		}
		a.marks, a.files = append(a.marks, m), append(a.files, e.Name())
	}
	return a, nil
}
