// Package omp integrates omp.
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Omp. Its attribution wiring is a committed hook file at .omp/hooks/pre/terma.ts
// that hands session lifecycle and file edits to `terma hook omp-*`; telemetry export
// lives in the user-scope extension `terma connect omp` writes into ~/.omp/agent/hooks/pre.
type Agent struct{}

func (Agent) Name() string                       { return "omp" }
func (Agent) DisplayName() string                { return "Omp" }
func (Agent) Installed(ctx context.Context) bool { return harness.Omp{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hookmgr.OmpHooksPath }
func (Agent) Default(root string) bool           { return hookmgr.HasOmp(root) }

func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanOmpHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"omp-session-start": sessionStart,
		"omp-session-end":   sessionEnd,
		"omp-file-edit":     fileEdit,
		"omp-prompt":        hookrun.ExtensionPrompt,
	}
}

func (Agent) FlushAfter() []string { return []string{"omp-session-end"} }

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return harness.Omp{} }

var (
	_ agents.Exporting = Agent{}
	_ agents.Agent     = Agent{}
)
