// Package opencode integrates OpenCode.
package opencode

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// Agent is OpenCode, whose events come from terma's user-scope plugin; it has no
// repository-scope hooks.
type Agent struct{}

func (Agent) Name() string                       { return "opencode" }
func (Agent) DisplayName() string                { return "OpenCode" }
func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return "" }
func (Agent) Default(string) bool                { return false }
func (Agent) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"opencode-session-start": sessionStart,
		"opencode-session-end":   sessionEnd,
		"opencode-file-edit":     fileEdit,
	}
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
