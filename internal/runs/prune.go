package runs

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
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
	keep := maps.Clone(queued)
	for id := range queued {
		if canon, err := aliasTarget(stateDir, id); err == nil {
			keep[canon] = true
		}
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if info, err := e.Info(); err != nil || now.Sub(info.ModTime()) <= Retention {
			continue
		}
		path := filepath.Join(dir, e.Name())
		switch name := e.Name(); {
		case e.IsDir() && strings.HasSuffix(name, asideExt):
			pruneAside(stateDir, name, now, keep)
		case !e.IsDir() && strings.HasSuffix(name, ".json"):
			pruneLedger(stateDir, path, now, keep)
		case !e.IsDir() && strings.HasSuffix(name, ".lock"):
			if _, err := os.Lstat(strings.TrimSuffix(path, ".lock")); err != nil {
				if unlock, err := flock.TryLock(path); err == nil {
					flock.Remove(path, unlock)
				}
			}
		case !e.IsDir() && strings.HasPrefix(name, config.TempPrefix):
			_ = os.Remove(path)
		}
	}
	pruneAliases(stateDir, now, keep)
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
	if raw, ok, rerr := load(path); err != nil && rerr == nil && ok {
		l = raw
	}
	if queued[sessionID] || slices.ContainsFunc(l.Aliases, func(a string) bool { return queued[a] }) ||
		(err != nil && now.Sub(info.ModTime()) <= MixedRetention) || (err == nil && now.Sub(l.lastActive(info.ModTime())) <= l.retention()) {
		unlock()
		return
	}
	if cur, aerr := aliasTarget(stateDir, sessionID); aerr == nil && cur == sessionID {
		if _, serr := os.Lstat(asidePath(path)); errors.Is(serr, fs.ErrNotExist) {
			for _, alias := range l.Aliases {
				removeAliasIf(stateDir, alias, "", func(target string) bool { return alias != sessionID && target == sessionID })
			}
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
	if info, err := os.Lstat(dir); (loadErr == nil && now.Sub(a.last()) <= MixedRetention) || err != nil || now.Sub(info.ModTime()) <= MixedRetention {
		return
	}
	age := MixedRetention
	if loadErr == nil {
		for _, f := range a.files {
			_ = os.Remove(filepath.Join(dir, f))
		}
		age = Retention
	}
	cleanAsideEntries(dir, now, age, loadErr == nil)
	_ = os.Remove(dir)
}
func cleanAsideEntries(dir string, now time.Time, age time.Duration, tempsOnly bool) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); (!tempsOnly || strings.HasPrefix(e.Name(), config.TempPrefix)) && err == nil && now.Sub(info.ModTime()) > age {
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
func pruneAliases(stateDir string, now time.Time, queued map[string]bool) {
	entries, _ := os.ReadDir(filepath.Join(stateDir, Dir, aliasesDir))
	for _, e := range entries {
		if info, err := e.Info(); err != nil || e.IsDir() || now.Sub(info.ModTime()) <= Retention || queued[e.Name()] {
			continue
		}
		if p := aliasPath(stateDir, e.Name()); strings.HasPrefix(e.Name(), config.TempPrefix) {
			_ = os.Remove(p)
		} else if b, err := os.ReadFile(p); err == nil {
			var canon string
			if t := strings.TrimSpace(string(b)); session.ValidID(t) {
				if canon, err = aliasTarget(stateDir, t); err != nil || queued[canon] {
					continue
				}
			}
			removeAliasIf(stateDir, e.Name(), canon, func(cur string) bool {
				if info, err := os.Lstat(p); err != nil || now.Sub(info.ModTime()) <= Retention {
					return false
				}
				if !session.ValidID(cur) {
					return canon == ""
				}
				resolved, err := aliasTarget(stateDir, cur)
				if err != nil || resolved != canon || queued[resolved] {
					return false
				}
				cp := filepath.Join(stateDir, Dir, resolved+".json")
				_, jerr := os.Lstat(cp)
				_, aerr := os.Lstat(asidePath(cp))
				return errors.Is(jerr, fs.ErrNotExist) && errors.Is(aerr, fs.ErrNotExist)
			})
		}
	}
}
func removeAliasIf(stateDir, alias, canon string, match func(target string) bool) {
	if !session.ValidID(alias) {
		return
	}
	ids := []string{alias}
	if canon != "" && canon != alias {
		ids = append(ids, canon)
		slices.Sort(ids)
	}
	tryLockAll(stateDir, ids, func() {
		p := aliasPath(stateDir, alias)
		if raw, err := os.ReadFile(p); err == nil && match(strings.TrimSpace(string(raw))) {
			_ = os.Remove(p)
		}
	})
}
func tryLockAll(stateDir string, ids []string, fn func()) {
	if len(ids) == 0 {
		fn()
		return
	}
	p := filepath.Join(stateDir, Dir, ids[0]+".json")
	u, err := flock.TryLock(p + ".lock")
	if err != nil {
		return
	}
	defer func() {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			flock.Remove(p+".lock", u)
		} else {
			u()
		}
	}()
	tryLockAll(stateDir, ids[1:], fn)
}
