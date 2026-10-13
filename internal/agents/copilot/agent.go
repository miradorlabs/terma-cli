// Package copilot integrates GitHub Copilot: the Copilot CLI and Copilot in VS Code.
package copilot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

const (
	name = "copilot"
	// command is the Copilot CLI as a developer types it.
	command = "copilot"
)

// Agent is GitHub Copilot's CLI and its agents in VS Code. Both read one user-level hooks
// directory and export GenAI telemetry of their own: VS Code from its user settings, the
// CLI from its environment alone, so setup hands the CLI's start to `terma launch`.
type Agent struct {
	// ConfigDir is terma's config directory, where the record of what setup replaced in
	// VS Code's settings is kept.
	ConfigDir string
}

func (Agent) Name() string        { return name }
func (Agent) DisplayName() string { return "GitHub Copilot" }

// Installed looks for the CLI, or a VS Code whose Copilot has run on this machine.
func (Agent) Installed(context.Context) bool {
	if _, err := exec.LookPath(command); err == nil {
		return true
	}
	for _, dir := range vscodeUserDirs() {
		if info, err := os.Stat(filepath.Join(dir, "globalStorage", "github.copilot-chat")); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// Surfaces is Copilot as one choice: the CLI and VS Code share the hooks directory.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: "GitHub Copilot CLI & VS Code", Installed: a.Installed}}
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"copilot-session-start": sessionStart,
		"copilot-prompt":        prompt,
		"copilot-post-tool-use": postToolUse,
		"copilot-stop":          stop,
		"copilot-session-end":   sessionEnd,
	}
}

// FlushAfter names the turn's end as well as the session's: VS Code's own agent never
// ends a session.
func (Agent) FlushAfter() []string { return []string{"copilot-stop", "copilot-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "native OTLP through the local relay only; AI credits, no cost; the CLI exports only when started through the shell function setup writes"}
}

var (
	_ agents.Covered  = Agent{}
	_ agents.Surfaced = Agent{}
	_ agents.Agent    = Agent{}
)
