// Package omp integrates omp.
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "omp"
	displayName = "Omp"
)

// Agent is Omp: attribution through a committed hook file, telemetry through a
// user-scope extension.
type Agent struct {
	relayexport.Own
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hooksPath }
func (Agent) Default(root string) bool           { return hasConfig(root) }

func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return hookrun.Extension{Tool: "omp"}.Events("omp")
}

func (Agent) FlushAfter() []string { return []string{"omp-session-end"} }

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return exporter{} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "tokens, effort, service tier and latency ride omp's native OTLP spans; estimated cost is posted as a companion record the server joins by session"}
}

var (
	_ agents.Covered   = Agent{}
	_ agents.Exporting = Agent{}
	_ agents.Agent     = Agent{}
)
