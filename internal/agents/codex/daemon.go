package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Codex's app-server daemon (0.157+ runs the interactive TUI's threads in it by
// default, and Codex Desktop's) reads the [otel] exporter once, when it starts: a
// config change reaches its threads only after `codex app-server daemon restart`
// (docs/RELAY.md). Its record is $CODEX_HOME/app-server-daemon/daemon.pid:
// {"pid":…, "processIdentity":{"startSeconds":…}, …}.

// CodexDaemon is a running Codex app-server daemon.
type CodexDaemon struct {
	PID     int
	Started time.Time
}

// RunningCodexDaemon reports the daemon for $CODEX_HOME, if its record names a live
// process.
func RunningCodexDaemon() (CodexDaemon, bool) {
	home, err := codexHome()
	if err != nil {
		return CodexDaemon{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if err != nil || len(data) > 64<<10 {
		return CodexDaemon{}, false
	}
	var rec struct {
		PID      int `json:"pid"`
		Identity struct {
			StartSeconds int64 `json:"startSeconds"`
		} `json:"processIdentity"`
	}
	if json.Unmarshal(data, &rec) != nil || rec.PID <= 1 || rec.Identity.StartSeconds <= 0 {
		return CodexDaemon{}, false
	}
	if !harness.ProcessAlive(rec.PID) {
		return CodexDaemon{}, false
	}
	return CodexDaemon{PID: rec.PID, Started: time.Unix(rec.Identity.StartSeconds, 0)}, true
}
