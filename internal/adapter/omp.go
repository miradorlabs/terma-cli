package adapter

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// omp is Omp. Its attribution wiring is a committed hook file at .omp/hooks/pre/terma.ts
// that hands session lifecycle and file edits to `terma hook omp-*`; telemetry export
// lives in the user-scope extension `terma connect omp` writes into ~/.omp/agent/hooks/pre.
type omp struct{}

func (omp) Name() string                       { return "omp" }
func (omp) DisplayName() string                { return "Omp" }
func (omp) Installed(ctx context.Context) bool { return harness.Omp{}.Detect(ctx).Found }
func (omp) HooksPath() string                  { return hookmgr.OmpHooksPath }
func (omp) Default(root string) bool           { return hookmgr.HasOmp(root) }

func (omp) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanOmpHooks(root, install)
}

func (omp) Events() map[string]Handler {
	return map[string]Handler{
		"omp-session-start": hookrun.OmpSessionStart,
		"omp-session-end":   hookrun.OmpSessionEnd,
		"omp-file-edit":     hookrun.OmpFileEdit,
	}
}

func (omp) FlushAfter() []string { return []string{"omp-session-end"} }
