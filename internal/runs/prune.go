package runs

import (
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

// Prune removes each ledger idle past its retention that no queued record names, with its
// lock and the alias files naming it, and what a crash left. queued are the session and
// subagent IDs records in the spool and the relay outbox name. A ledger whose lock is held
// waits for the next prune. The relay runs it hourly, and teardown once both queues are gone.
func Prune(stateDir string, now time.Time, queued map[string]bool) {
	dir := filepath.Join(stateDir, Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		// Every write is activity, so a file written since cannot have been idle that long.
		info, err := e.Info()
		if err != nil || e.IsDir() || now.Sub(info.ModTime()) <= Retention {
			continue
		}
		path := filepath.Join(dir, e.Name())
		switch name := e.Name(); {
		case strings.HasSuffix(name, ".json"):
			pruneLedger(stateDir, path, now, queued)
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

// pruneLedger removes the ledger at path, with its lock and alias files, unless a writer
// holds it, it was active within its retention, or a queued record names its session or an
// alias. An unreadable one goes by its file's time alone: it may record a crossing.
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
	l, _, err := load(path)
	if err == nil && (now.Sub(l.lastActive(info.ModTime())) <= l.retention() || queued[sessionID] ||
		slices.ContainsFunc(l.Aliases, func(a string) bool { return queued[a] })) {
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

// retention is how long l is kept after its last activity: MixedRetention while its
// current run is Mixed, else Retention.
func (l ledger) retention() time.Duration {
	if n := len(l.Runs); n > 0 && !l.Runs[n-1].MixedAt.IsZero() {
		return MixedRetention
	}
	return Retention
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
