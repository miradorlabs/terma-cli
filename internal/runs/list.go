package runs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Mixed lists, sorted, the sessions whose run in force at now is Mixed (MixedAt), and so
// send no telemetry until they start a new run, as doctor shows them: those active within
// recent of now (ledger.Active), and the number of the others the ledger still keeps. A
// ledger that cannot be read counts, as MixedAt has it, as active.
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
