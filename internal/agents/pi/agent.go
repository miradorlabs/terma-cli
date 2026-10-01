// Package pi integrates Pi.
package pi

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Pi (@earendil-works/pi-coding-agent), whose events come from terma's
// user-scope extension; it has no repository-scope hooks.
type Agent struct{ relayexport.Own }

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

func (Agent) Events() map[string]agents.Handler { return hookrun.Extension{Tool: "pi"}.Events("pi") }

func (Agent) FlushAfter() []string { return []string{"pi-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "Pi has no OTLP export; terma's extension exports tokens, cost and tool calls through the local relay only"}
}

var (
	_ agents.Covered = Agent{}
	_ agents.Agent   = Agent{}
)
