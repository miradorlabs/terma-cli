// Package pi integrates Pi.
package pi

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Pi (@earendil-works/pi-coding-agent). Like OpenCode it has no repository-scope
// hooks: terma's user-scope extension (internal/harness/pi/terma.ts) calls the binary
// with the events below. It is an adapter so `terma hook pi-*` dispatches from the same
// table as everyone else's.
type Agent struct{}

func (Agent) Name() string        { return "pi" }
func (Agent) DisplayName() string { return "Pi" }
func (Agent) Installed(context.Context) bool {
	_, err := exec.LookPath("pi")
	return err == nil
}
func (Agent) HooksPath() string   { return "" }
func (Agent) Default(string) bool { return false }
func (Agent) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"pi-session-start": hookrun.PiSessionStart,
		"pi-prompt":        hookrun.PiPrompt,
		"pi-session-end":   hookrun.PiSessionEnd,
		"pi-file-edit":     hookrun.PiFileEdit,
	}
}

func (Agent) FlushAfter() []string { return []string{"pi-session-end"} }

var (
	_ agents.Agent = Agent{}
)
