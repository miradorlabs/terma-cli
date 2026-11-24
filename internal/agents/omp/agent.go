// Package omp integrates OMP: its native OTLP exporter, pointed at the local
// relay through OMP's .env, and terma's hooks-only extension, which reports sessions and
// file edits to `terma hook omp-*` for commit attribution, and an interactive session's
// name to omp-title (title.go).
package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "omp"
	displayName = "OMP"
)

// Agent is OMP: telemetry from its native exporter, attribution from terma's extension.
type Agent struct {
	// ConfigDir is terma's config directory, which holds the journal of omp's .env.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

// Installed reports whether an omp terma can set up is on the PATH: one before OMP 18.18.2 is
// not, so setup --yes leaves it out rather than stopping at its refusal.
func (Agent) Installed(ctx context.Context) bool {
	if _, old := tooOld(ctx); old {
		return false
	}
	return exporter{}.Detect(ctx).Found
}

// Surfaces is OMP as one choice, marked beta at setup and naming the first OMP terma sets up.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: displayName + " " + minimumVersion + "+ (beta)", Installed: a.Installed}}
}

// Events are the extension's omp-* hooks.
func (Agent) Events() map[string]agents.Handler {
	events := hookrun.Extension{Tool: name, ClaimParent: true}.Events(name)
	events["omp-title"] = title
	return events
}

// FlushAfter delivers the spool when a session ends, and when a session is named: OMP names
// it after the first reply, and a long session would otherwise hold its name until its end.
func (Agent) FlushAfter() []string { return []string{"omp-session-end", "omp-title"} }

// Harness is how terma configures the agent's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "tokens, estimated cost, tool calls and subagents ride OMP's native OTLP spans to the local relay; terma's extension exports nothing and reports sessions, edit and write tool changes, subagent parents and an interactive session's name"}
}

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string { return []string{ompTitleStateDir} }

// ContentConsented is the consent session titles travel under.
func (Agent) ContentConsented(c hookrun.Consent) bool { return titleConsented(c) }

// RefreshMachine rewrites an installed OMP extension with this build's template.
func (Agent) RefreshMachine() (string, bool, error) { return refreshExtension() }

var (
	_ agents.Covered          = Agent{}
	_ agents.Exporting        = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.Surfaced         = Agent{}
	_ agents.StateKeeper      = Agent{}
	_ agents.ContentConsent   = Agent{}
	_ agents.MachineRefresher = Agent{}
)
