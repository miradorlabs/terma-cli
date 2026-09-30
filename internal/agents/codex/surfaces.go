package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// desktop is Codex Desktop's surface name: its threads run in codex app-server, and
// reach Terma through the relay and the repository's trusted hooks.
const desktop = "codex-desktop"

// Surfaces are Codex's CLI and its desktop app, chosen apart at setup.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{
		{Name: "codex", DisplayName: "Codex CLI", Installed: a.Installed},
		{
			Name: desktop, DisplayName: "Codex Desktop", Installed: desktopInstalled,
			Needs: agents.Needs{Signals: []string{"logs"}, Hooks: true},
			InstallSteps: []string{"Approve Codex Desktop capture:\n" +
				"a. Open this repository in Codex Desktop and trust the project if prompted.\n" +
				"b. Open Settings → Hooks, then select Review for the entries from .codex/hooks.json.\n" +
				"c. Inspect and approve each Terma hook command for full capture. Codex CLI is not required.\n" +
				"d. Run `terma desktop status` to confirm 'Codex hooks: ready', then start a new Local task in this repository."},
			SetupSteps: []string{"Codex Desktop: in a connected repository, open Settings → Hooks → Review in Codex Desktop and approve Terma's hooks."},
			Reports:    "reports through the relay and this repository's hooks",
			Warn: func() string {
				if global, err := (harness.Codex{}).Status(); err == nil && global.Connected {
					return "also has a user-level exporter; it may send Desktop activity from other repositories"
				}
				return ""
			},
		},
	}
}

// desktopInstalled finds the ChatGPT app Codex Desktop ships in (macOS only).
func desktopInstalled(context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{"/Applications/ChatGPT.app", filepath.Join(home, "Applications", "ChatGPT.app")} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
