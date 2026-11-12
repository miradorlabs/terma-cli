// Package copilot integrates GitHub Copilot: the Copilot CLI and Copilot in VS Code.
package copilot

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name = "copilot"
	// command is the Copilot CLI as a developer types it.
	command = "copilot"
)

// Agent is GitHub Copilot's CLI and Copilot in VS Code. Both read one user-level hooks
// directory and run the Copilot runtime, which exports GenAI telemetry of its own: in VS
// Code's agent host from VS Code's user settings, in the CLI from its environment alone,
// so setup hands the CLI's start to `terma launch`.
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
	return len(vscodeUserDirs(false)) > 0
}

// Surfaces is Copilot as one choice, marked beta at setup: the CLI and VS Code share the
// hooks directory.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: "Copilot (beta)", Installed: a.Installed}}
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
	return agents.CapabilitySupport{Level: agents.SupportPartial, Note: "files its edit tools write; a file a shell command writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "native OTLP through the local relay only; AI credits, no cost; the CLI exports when started through the shell function setup writes, VS Code from its agent host, and Copilot Chat's own agent in VS Code not at all"}
}

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string { return []string{copilotTitleStateDir} }

// ContentConsented is the consent session titles travel under.
func (Agent) ContentConsented(c hookrun.Consent) bool { return titleConsented(c) }

var (
	_ agents.Covered        = Agent{}
	_ agents.Surfaced       = Agent{}
	_ agents.Agent          = Agent{}
	_ agents.StateKeeper    = Agent{}
	_ agents.ContentConsent = Agent{}
)
