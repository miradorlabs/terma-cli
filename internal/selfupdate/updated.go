package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// updatedFile records the last release terma installed by itself, beside the check.
const updatedFile = "updated.json"

// Updated is the last release terma installed by itself, the relay's in the background or
// after a command: a relay prints nothing, so the next command says so once (TellUpdated),
// and doctor names it.
type Updated struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
	// Through is the package manager that installed it, "Homebrew" or "npm"; "" in place.
	Through string `json:"through,omitempty"`
	// Told means a command has said so.
	Told bool `json:"told,omitempty"`
}

// LoadUpdated reads the last automatic update from the state directory dir; false when there
// was none.
func LoadUpdated(dir string) (Updated, bool) {
	var u Updated
	data, err := os.ReadFile(statePath(dir, updatedFile))
	if err != nil || json.Unmarshal(data, &u) != nil || u.To == "" {
		return Updated{}, false
	}
	return u, true
}

func saveUpdated(dir string, u Updated) {
	_ = config.WriteJSON(statePath(dir, updatedFile), u, 0o600)
}

// How names how u was installed, for the developer: "Homebrew", "npm" or "in place".
func (u Updated) How() string {
	if u.Through == "" {
		return "in place"
	}
	return u.Through
}

// TellUpdated is the line saying, once, that terma updated itself in the background since
// the last command; "" when it did not, or that was said. Under the update lock, so a relay
// recording an update meanwhile is not marked told unsaid; while another holds it, it says
// nothing this time.
func TellUpdated(dir string) string {
	unlock, err := Lock(dir)
	if err != nil {
		return ""
	}
	defer unlock()
	u, ok := LoadUpdated(dir)
	if !ok || u.Told {
		return ""
	}
	u.Told = true
	saveUpdated(dir, u)
	return fmt.Sprintf("terma updated itself in the background: %s → %s (%s).", u.From, u.To, u.How())
}
