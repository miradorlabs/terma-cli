package runs

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// MixedRun is a session whose run in force is Mixed (for doctor).
type MixedRun struct {
	Session string
	Why     Why
	Recent  bool
}

// Mixed lists sessions whose run in force at now is Mixed (unreadable ledgers fail closed).
func Mixed(stateDir string, now time.Time, recent time.Duration) []MixedRun {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil
	}
	written := map[string]time.Time{}
	for _, e := range entries {
		if id, ok := sessionOf(e.Name()); ok && e.IsDir() == strings.HasSuffix(e.Name(), asideExt) {
			var at time.Time
			if info, err := e.Info(); err == nil {
				at = info.ModTime()
			}
			written[id] = latest(written[id], at)
		}
	}
	seen := map[string]MixedRun{}
	for id, at := range written {
		canon := id
		if t, err := aliasTarget(stateDir, id); err == nil {
			canon = t
		}
		if l, found, _, err := resolve(stateDir, id); err != nil {
			seen[canon] = MixedRun{Session: canon, Recent: true}
		} else if found && len(l.Runs) > 0 {
			if r := inForce(l.Runs, now); !r.MixedAt.IsZero() && !seen[canon].Recent {
				seen[canon] = MixedRun{Session: canon, Why: r.why(), Recent: now.Sub(l.lastActive(at)) <= recent}
			}
		}
	}
	out := slices.Collect(maps.Values(seen))
	slices.SortFunc(out, func(a, b MixedRun) int { return strings.Compare(a.Session, b.Session) })
	return out
}

func sessionOf(name string) (string, bool) {
	if id, ok := strings.CutSuffix(name, ".json"); ok && session.ValidID(id) {
		return id, true
	}
	id, ok := strings.CutSuffix(name, asideExt)
	return id, ok && session.ValidID(id)
}
