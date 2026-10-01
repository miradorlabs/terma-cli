package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// Codex's app-server daemon reads [otel] only when it starts, so a config change reaches
// its threads only after `codex app-server daemon restart`.

// daemon is a running Codex app-server daemon.
type daemon struct {
	PID     int
	Started time.Time
}

// runningDaemon reports the daemon for $CODEX_HOME if its record names a live process.
func runningDaemon() (daemon, bool) {
	home, err := codexHome()
	if err != nil {
		return daemon{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if err != nil || len(data) > 64<<10 {
		return daemon{}, false
	}
	var rec struct {
		PID      int `json:"pid"`
		Identity struct {
			StartSeconds int64 `json:"startSeconds"`
		} `json:"processIdentity"`
	}
	if json.Unmarshal(data, &rec) != nil || rec.PID <= 1 || rec.Identity.StartSeconds <= 0 {
		return daemon{}, false
	}
	if !procinfo.Alive(rec.PID) {
		return daemon{}, false
	}
	return daemon{PID: rec.PID, Started: time.Unix(rec.Identity.StartSeconds, 0)}, true
}
