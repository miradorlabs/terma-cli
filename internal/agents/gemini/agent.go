// Package gemini integrates Gemini CLI.
package gemini

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// Agent is Gemini CLI, whose hooks come from terma's user-level extension: it fires in
// every folder, where a repository's own hooks need the folder trusted.
type Agent struct{}

func (Agent) Name() string        { return "gemini" }
func (Agent) DisplayName() string { return "Gemini CLI" }
func (Agent) Installed(context.Context) bool {
	_, err := exec.LookPath("gemini")
	return err == nil
}
func (Agent) HooksPath() string   { return "" }
func (Agent) Default(string) bool { return false }
func (Agent) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"gemini-session-start": sessionStart,
		"gemini-prompt":        prompt,
		"gemini-after-tool":    afterTool,
		"gemini-session-end":   sessionEnd,
	}
}

func (Agent) FlushAfter() []string { return []string{"gemini-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "native OTLP through the local relay only; no cost"}
}

var (
	_ agents.Covered = Agent{}
	_ agents.Agent   = Agent{}
)
