// Package gemini integrates Gemini CLI.
package gemini

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
)

// Agent is Gemini CLI. Its hooks come from terma's user-level Gemini extension
// (harness.ConnectGeminiRelay), which fires in every folder; a repository's own
// .gemini/settings.json hooks need the folder trusted. It is an adapter so `terma hook
// gemini-*` dispatches from the same table as everyone else's.
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

var (
	_ agents.Agent = Agent{}
)
