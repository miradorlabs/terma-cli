package runs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Mixed lists, sorted, the sessions whose run in force at now is Mixed (MixedAt): those
// that send no telemetry until they start a new run, as doctor shows them. A ledger that
// cannot be read counts, as MixedAt has it.
func Mixed(stateDir string, now time.Time) []string {
	entries, err := os.ReadDir(filepath.Join(stateDir, Dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		if _, mixed := MixedAt(stateDir, id, now); mixed {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
