package runs

import (
	"cmp"
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
	// At is when the run went Mixed; zero when its ledger could not be read.
	At     time.Time
	Recent bool
}

// Mixed lists sessions whose run in force at now is Mixed (unreadable ledgers fail closed).
func Mixed(stateDir string, now time.Time, recent time.Duration) []MixedRun {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil
	}
	written, seen := map[string]time.Time{}, map[string]MixedRun{}
	for _, e := range entries {
		if id, ok := sessionOf(e.Name()); ok && e.IsDir() == strings.HasSuffix(e.Name(), asideExt) {
			at := time.Time{}
			if info, err := e.Info(); err == nil {
				at = info.ModTime()
			}
			written[id] = latest(written[id], at)
		}
	}
	for id, at := range written {
		t, _ := aliasTarget(stateDir, id)
		canon := cmp.Or(t, id)
		if l, found, _, err := resolve(stateDir, id); err != nil {
			seen[canon] = MixedRun{Session: canon, Recent: true}
		} else if found && len(l.Runs) > 0 {
			if r := inForce(l.Runs, now); !r.MixedAt.IsZero() && !seen[canon].Recent {
				seen[canon] = MixedRun{Session: canon, Why: r.why(), At: r.MixedAt, Recent: now.Sub(l.lastActive(at)) <= recent}
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
