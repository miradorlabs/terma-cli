package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Surfaces is Claude Code as one choice: the CLI and the desktop app read the same
// user-level settings.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{
		Name: name, DisplayName: "Claude Code & Desktop",
		Installed: func(ctx context.Context) bool { return a.Installed(ctx) || desktopInstalled() },
	}}
}

func desktopInstalled() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{"/Applications/Claude.app", filepath.Join(home, "Applications", "Claude.app")} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
