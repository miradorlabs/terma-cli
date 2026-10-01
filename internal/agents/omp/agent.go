// Package omp integrates omp.
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Omp. Its attribution wiring is a committed hook file at .omp/hooks/pre/terma.ts
// that hands session lifecycle and file edits to `terma hook omp-*`; telemetry export
// lives in the user-scope extension `terma connect omp` writes into ~/.omp/agent/hooks/pre.
type Agent struct{}

func (Agent) Name() string                       { return "omp" }
func (Agent) DisplayName() string                { return "Omp" }
func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hooksPath }
func (Agent) Default(root string) bool           { return hasConfig(root) }

func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"omp-session-start": sessionStart,
		"omp-session-end":   sessionEnd,
		"omp-file-edit":     fileEdit,
		"omp-prompt":        hookrun.TurnStart,
	}
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
