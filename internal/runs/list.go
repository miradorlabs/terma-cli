package runs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// MixedRun is a session whose run in force is Mixed, as doctor lists it: why, and whether
// it was active within the recent window Mixed was asked for.
type MixedRun struct {
	Session string
	Why     Why
	Recent  bool
}

// Mixed lists, sorted by session, for doctor, the sessions whose run in force at now is
// Mixed, each with why and whether it was active within recent. A sidecar counts with its
// ledger, or alone (MarkAside), and an unreadable ledger or sidecar counts as recent and
// Mixed, as MixedAt has it, with no reason.
func Mixed(stateDir string, now time.Time, recent time.Duration) []MixedRun {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil
	}
	written := map[string]time.Time{}
	for _, e := range entries {
		id, ok := sessionOf(e.Name())
		if !ok || e.IsDir() != strings.HasSuffix(e.Name(), asideExt) {
			continue
		}
		var at time.Time
		if info, err := e.Info(); err == nil {
			at = info.ModTime()
		}
		written[id] = latest(written[id], at)
	}
	var out []MixedRun
	for id, at := range written {
		l, found, _, err := resolve(stateDir, id)
		switch {
		case err != nil:
			out = append(out, MixedRun{Session: id, Recent: true})
			continue
		case !found || len(l.Runs) == 0:
			continue
		}
		if r := inForce(l.Runs, now); !r.MixedAt.IsZero() {
			out = append(out, MixedRun{Session: id, Why: r.why(), Recent: now.Sub(l.lastActive(at)) <= recent})
		}
	}
	slices.SortFunc(out, func(a, b MixedRun) int { return strings.Compare(a.Session, b.Session) })
	return out
}

// sessionOf is the session whose ledger or sidecar is named name; false for any other file.
func sessionOf(name string) (string, bool) {
	for _, ext := range []string{".json", asideExt} {
		if id, ok := strings.CutSuffix(name, ext); ok && session.ValidID(id) {
			return id, true
		}
	}
	return "", false
}
