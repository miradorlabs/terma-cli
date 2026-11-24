// Package pi integrates Pi, which has no OpenTelemetry of its own: terma's extension
// (terma.ts) is its exporter, sending chat and tool spans and prompt logs to the local
// relay, and hands sessions, prompts and file edits to `terma hook pi-*` for commit
// attribution.
package pi

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
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

func (Agent) Events() map[string]agents.Handler { return hookrun.Extension{Tool: "pi"}.Events("pi") }

func (Agent) FlushAfter() []string { return []string{"pi-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "Pi has no OTLP export; terma's extension exports tokens, cost and tool calls through the local relay only"}
}

// RefreshMachine rewrites an installed Pi extension with this build's template.
func (Agent) RefreshMachine() (string, bool, error) { return refreshExtension() }

var (
	_ agents.Covered          = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.MachineRefresher = Agent{}
)
