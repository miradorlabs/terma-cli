// Package opencode integrates OpenCode.
package opencode

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "opencode"
	displayName = "OpenCode"
)

// Agent is OpenCode, whose events come from terma's user-scope plugin; it has no
// repository-scope hooks.
type Agent struct{}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }

func (Agent) Events() map[string]agents.Handler {
	events := hookrun.Extension{Tool: "opencode", Source: "session.created"}.Events("opencode")
	delete(events, "opencode-prompt") // the plugin calls no prompt hook
	return events
}

func (Agent) FlushAfter() []string { return []string{"opencode-session-end"} }

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return exporter{} }

// RefreshMachine rewrites the plugin.
func (Agent) RefreshMachine() (string, bool, error) { return exporter{}.RefreshPlugin() }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull}
}

var (
	_ agents.Covered          = Agent{}
	_ agents.MachineRefresher = Agent{}
	_ agents.Exporting        = Agent{}
	_ agents.Agent            = Agent{}
)
