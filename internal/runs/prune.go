package runs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Prune removes ledgers idle past retention that no queued record names (along with locks
// and stale aliases). Sidecars are pruned only after their ledger is gone and MixedRetention
// has elapsed since their last mark. Stops when ctx ends.
func Prune(ctx context.Context, stateDir string, now time.Time, queued map[string]bool) {
	dir := filepath.Join(stateDir, Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= Retention {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if strings.HasSuffix(e.Name(), asideExt) {
				pruneAside(stateDir, e.Name(), now, queued)
			}
			continue
		}
		switch name := e.Name(); {
		case strings.HasSuffix(name, ".json"):
			pruneLedger(stateDir, path, now, queued)
		case strings.HasSuffix(name, ".lock"):
			if _, err := os.Lstat(strings.TrimSuffix(path, ".lock")); err == nil {
				continue
			}
			if unlock, err := flock.TryLock(path); err == nil {
				flock.Remove(path, unlock)
			}
		case strings.HasPrefix(name, config.TempPrefix):
			_ = os.Remove(path)
		}
	}
	pruneAliases(stateDir, now)
}

func pruneLedger(stateDir, path string, now time.Time, queued map[string]bool) {
	lock := path + ".lock"
	unlock, err := flock.TryLock(lock)
	if err != nil {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || now.Sub(info.ModTime()) <= Retention {
		unlock()
		return
	}
	sessionID := strings.TrimSuffix(filepath.Base(path), ".json")
	l, _, _, err := resolve(stateDir, sessionID)
	if err != nil {
		if queued[sessionID] || now.Sub(info.ModTime()) <= MixedRetention {
			unlock()
			return
		}
	} else if now.Sub(l.lastActive(info.ModTime())) <= l.retention() || queued[sessionID] ||
		slices.ContainsFunc(l.Aliases, func(a string) bool { return queued[a] }) {
		unlock()
		return
	}
	for _, alias := range l.Aliases {
		if b, err := os.ReadFile(aliasPath(stateDir, alias)); err == nil && string(b) == sessionID {
			_ = os.Remove(aliasPath(stateDir, alias))
		}
	}
	_ = os.Remove(path)
	flock.Remove(lock, unlock)
}

func pruneAside(stateDir, name string, now time.Time, queued map[string]bool) {
	sessionID, ok := sessionOf(name)
	if !ok {
		return
	}
	dir := filepath.Join(stateDir, Dir, name)
	if _, err := os.Lstat(filepath.Join(stateDir, Dir, sessionID+".json")); !errors.Is(err, fs.ErrNotExist) || queued[sessionID] {
		cleanAsideEntries(dir, now, Retention, true)
		return
	}
	a, loadErr := loadAside(dir)
	if loadErr == nil && now.Sub(a.last()) <= MixedRetention {
		return
	}
	if info, err := os.Lstat(dir); err != nil || now.Sub(info.ModTime()) <= MixedRetention {
		return
	}
	if loadErr == nil {
		for _, f := range a.files {
			_ = os.Remove(filepath.Join(dir, f))
		}
		cleanAsideEntries(dir, now, Retention, true)
	} else {
		cleanAsideEntries(dir, now, MixedRetention, false)
	}
	_ = os.Remove(dir) // succeeds only when empty
}

func cleanAsideEntries(dir string, now time.Time, age time.Duration, tempsOnly bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if tempsOnly && !strings.HasPrefix(e.Name(), config.TempPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (l ledger) retention() time.Duration {
	if n := len(l.Runs); n > 0 && !l.Runs[n-1].MixedAt.IsZero() {
		return MixedRetention
	}
	return Retention
}

func pruneAliases(stateDir string, now time.Time) {
	dir := filepath.Join(stateDir, Dir, aliasesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || now.Sub(info.ModTime()) <= Retention {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), config.TempPrefix) {
			_ = os.Remove(path)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if ledger, err := ledgerPath(stateDir, string(b)); err != nil {
			_ = os.Remove(path)
		} else if _, err := os.Lstat(ledger); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(path)
		}
	}
}
