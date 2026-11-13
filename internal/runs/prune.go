package runs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Prune removes under the state directory stateDir every session ledger untouched for
// longer than Retention, with the alias files that still name it, and what a crash or a
// removed ledger left behind. A ledger whose lock is held is left for the next prune.
func Prune(stateDir string, now time.Time) {
	dir := filepath.Join(stateDir, Dir)
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
		switch name := e.Name(); {
		case strings.HasSuffix(name, ".json"):
			pruneLedger(stateDir, path, now)
		case strings.HasSuffix(name, ".lock"):
			// A lock's time is its creation, so an old one may still be in use.
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

// pruneLedger removes the ledger at path, if no writer holds it and it is still old,
// with its lock and the alias files that name it.
func pruneLedger(stateDir, path string, now time.Time) {
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
	if l, _, err := load(path); err == nil {
		for _, alias := range l.Aliases {
			if b, err := os.ReadFile(aliasPath(stateDir, alias)); err == nil && string(b) == sessionID {
				_ = os.Remove(aliasPath(stateDir, alias))
			}
		}
	}
	_ = os.Remove(path)
	flock.Remove(lock, unlock)
}

// pruneAliases removes alias files older than Retention whose session's ledger is gone.
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
