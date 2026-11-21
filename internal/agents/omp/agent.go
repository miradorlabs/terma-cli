// Package omp integrates omp (oh-my-pi): its native OTLP exporter, pointed at the local
// relay through omp's .env, and terma's Pi-family extension, which reports sessions and
// file edits to `terma hook omp-*` for commit attribution.
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "omp"
	displayName = "Omp"
)

// Agent is omp: telemetry from its native exporter, attribution from terma's extension.
type Agent struct {
	// ConfigDir is terma's config directory, which holds the journal of omp's .env.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

// Installed reports whether omp is on the PATH.
func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }

// Events are the extension's omp-* hooks.
func (Agent) Events() map[string]agents.Handler {
	return hookrun.Extension{Tool: name, ClaimParent: true}.Events(name)
}

// FlushAfter delivers the spool when a session ends.
func (Agent) FlushAfter() []string { return []string{"omp-session-end"} }

// Harness is how terma configures the agent's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "tokens, estimated cost, tool calls and subagents ride omp's native OTLP spans to the local relay; terma's extension reports only sessions and file edits"}
}

var (
	_ agents.Covered   = Agent{}
	_ agents.Exporting = Agent{}
	_ agents.Agent     = Agent{}
)
