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

// Crossings a hook could not write under the ledger lock are written lock-free to
// runs/<session>.mixed/<id>.json. Unlike claim sidecars, runs sidecars never expire;
// Prune keeps them with their ledger and for MixedRetention after their last mark. Reads
// check the sidecar before the ledger, and folds save the ledger before removing marks.
const (
	asideExt    = ".mixed"
	asideWrites = 3 // retries if a concurrent fold removed the emptied sidecar directory
)

type aside struct {
	marks []mark
	files []string // file names for fold to remove
}

type mark struct {
	Run string    `json:"run"`
	At  time.Time `json:"at"`
}

// MarkAside records a lock-free Mixed mark for sessionID/runID with reason Aside; an
// empty runID allocates a new run covering all earlier records, matching MarkMixed.
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

// apply marks l's runs Mixed per a's marks (earlier time wins).
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
		l.Runs[i].mark(m.At, Why{Reason: Aside})
		l.active(m.At)
	}
}

func (a aside) last() time.Time {
	var out time.Time
	for _, m := range a.marks {
		out = latest(out, m.At)
	}
	return out
}

// fold saves sessionID's sidecar marks into the locked ledger before removing them.
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
