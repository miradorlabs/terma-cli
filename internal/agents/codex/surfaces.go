package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Surfaces is Codex as one choice: the TUI, the desktop app and the IDE extension share
// ~/.codex's configuration, hooks and trust.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{
		Name: name, DisplayName: "Codex TUI & Desktop",
		Installed:    func(ctx context.Context) bool { return a.Installed(ctx) || desktopInstalled(ctx) },
		InstallSteps: []string{"Approve Terma's hooks in Codex: run `/hooks` in this repository (in the desktop app: Settings → Hooks → Review)."},
		SetupSteps:   []string{"Approve Terma's hooks in Codex: in a connected repository, run `/hooks` (in the desktop app: Settings → Hooks → Review)."},
		Reports:      "reports through the relay and this repository's hooks",
	}}
}

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
