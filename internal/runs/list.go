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
// were active within recent; older counts the rest the ledger keeps. An unreadable ledger
// counts as active and Mixed, as MixedAt has it.
func Mixed(stateDir string, now time.Time, recent time.Duration) (active []string, older int) {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil, 0
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		path := filepath.Join(stateDir, Dir, e.Name())
		l, found, err := load(path)
		switch {
		case err != nil:
			active = append(active, id)
			continue
		case !found || len(l.Runs) == 0 || inForce(l.Runs, now).MixedAt.IsZero():
			continue
		}
		var written time.Time
		if info, err := e.Info(); err == nil {
			written = info.ModTime()
		}
		if now.Sub(l.lastActive(written)) <= recent {
			active = append(active, id)
		} else {
			older++
		}
	}
	slices.Sort(active)
	return active, older
}
