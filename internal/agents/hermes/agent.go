// Package hermes integrates Hermes.
package hermes

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Hermes (Nous Research). Its shell hooks are user-level only and do not
// fire in its TUI, so terma's user-level plugin (internal/harness/hermes) calls the
// binary with the events below. It is an adapter so `terma hook hermes-*` dispatches
// from the same table as everyone else's.
type Agent struct{}

func (Agent) Name() string        { return "hermes" }
func (Agent) DisplayName() string { return "Hermes" }
func (Agent) Installed(context.Context) bool {
	_, err := exec.LookPath("hermes")
	return err == nil
}
func (Agent) HooksPath() string   { return "" }
func (Agent) Default(string) bool { return false }
func (Agent) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (Agent) Events() map[string]agents.Handler {
	return hookrun.Extension{Tool: "hermes"}.Events("hermes")
}

func (Agent) FlushAfter() []string { return []string{"hermes-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "Hermes has no usable OTLP export; terma's plugin exports tokens, cost and tool calls through the local relay only; auxiliary calls (titles, compression) fire no hook"}
}

var (
	_ agents.Covered = Agent{}
	_ agents.Agent   = Agent{}
)
