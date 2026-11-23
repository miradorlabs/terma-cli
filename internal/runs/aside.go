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
)

// Crossings a hook could not write under the ledger lock go lock-free to runs/<session>.mixed/<id>.json.
// Never expire; Prune keeps them with their ledger and MixedRetention after the last mark.
const asideExt, asideWrites = ".mixed", 3

type aside struct {
	marks []mark
	files []string // file names for fold to remove
}

type mark struct {
	Run string    `json:"run"`
	At  time.Time `json:"at"`
}

func asidePath(ledger string) string { return strings.TrimSuffix(ledger, ".json") + asideExt }

// MarkAside records a lock-free Mixed mark for sessionID/runID with reason Aside; an
// empty runID marks the latest run if a ledger exists, or starts a run covering earlier
// records when no ledger exists yet.
func MarkAside(stateDir, sessionID, runID string, at time.Time) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	dir := asidePath(path)
	if runID == "" {
		runID = newRunID()
	}
	b, err := json.Marshal(mark{Run: runID, At: at.UTC()})
	if err != nil {
		return err
	}
	name := newRunID()
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

func (a aside) last() (out time.Time) {
	for _, m := range a.marks {
		out = latest(out, m.At)
	}
	return out
}

// fold saves the ledger's sidecar marks into the locked ledger before removing them.
func fold(path string, l *ledger) error {
	dir := asidePath(path)
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

func loadAside(dir string) (aside, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
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
