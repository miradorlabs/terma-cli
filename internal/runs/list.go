package runs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Mixed lists, sorted, for doctor, the sessions whose run in force at now is Mixed and that
// were active within recent; older counts the rest the ledger keeps. A sidecar counts with
// its ledger, or alone (MarkAside), and an unreadable ledger or sidecar counts as active and
// Mixed, as MixedAt has it.
func Mixed(stateDir string, now time.Time, recent time.Duration) (active []string, older int) {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil, 0
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
	for id, at := range written {
		l, found, _, err := resolve(stateDir, id)
		switch {
		case err != nil:
			active = append(active, id)
			continue
		case !found || len(l.Runs) == 0 || inForce(l.Runs, now).MixedAt.IsZero():
			continue
		}
		if now.Sub(l.lastActive(at)) <= recent {
			active = append(active, id)
		} else {
			older++
		}
	}
	slices.Sort(active)
	return active, older
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
