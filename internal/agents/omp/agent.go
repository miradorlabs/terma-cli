// Package omp integrates omp.
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "omp"
	displayName = "Omp"
)

// Agent is Omp: telemetry through a user-scope extension.
type Agent struct {
	relayexport.Own
	// ConfigDir is terma's config directory, which holds the helper scripts.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }

func (Agent) Events() map[string]agents.Handler {
	return hookrun.Extension{Tool: "omp"}.Events("omp")
}

func (Agent) FlushAfter() []string { return []string{"omp-session-end"} }

// Harness is how terma configures the agent's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

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
