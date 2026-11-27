// Package pi integrates Pi, which has no OpenTelemetry of its own: terma's extension
// (terma.ts) is its exporter, sending model calls, tool calls, prompts, names and
// compactions to the local relay under scope terma-pi, and hands sessions, prompts, what a
// tool reaches and file edits to `terma hook pi-*` for commit attribution. extension.go
// writes and refreshes the extension, relay.go points it at the relay and says what keeps it
// from there, telemetry.go is what the relay withholds from its export, and
// minimum_version.go the first Pi it records in.
package pi

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "pi"
	displayName = "Pi"
)

// Agent is Pi (@earendil-works/pi-coding-agent), whose events come from terma's
// user-scope extension; it has no repository-scope hooks.
type Agent struct{}

func (Agent) Name() string        { return name }
func (Agent) DisplayName() string { return displayName }

// Installed reports whether a Pi terma can set up is on the PATH: one older than the
// minimum is not, so setup --yes leaves it out rather than stopping at its refusal.
func (Agent) Installed(ctx context.Context) bool {
	d := detect(ctx)
	if _, old := tooOldVersion(d.Version); old {
		return false
	}
	return d.Found
}

// DetectedVersion is the version of the Pi on PATH, for doctor; "" when it cannot tell.
func (Agent) DetectedVersion(ctx context.Context) string { return detect(ctx).Version }

// Surfaces is Pi as one choice, marked beta at setup and naming the first Pi terma sets up.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: displayName + " " + minimumVersion + "+ (beta)", Installed: a.Installed}}
}

// Events are the pi-* hooks the extension runs. It names a child process's parent itself,
// so no claim needs to.
func (Agent) Events() map[string]agents.Handler { return hookrun.Extension{Tool: name}.Events(name) }

func (Agent) FlushAfter() []string { return []string{"pi-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull, Note: "files its write and edit tools change; a file a shell command writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "model calls with tokens and Pi's own cost, replies, tool calls, MCP calls, child-process subagents, prompts, session names and compactions ride terma's extension to the local relay; its hooks claim sessions, guard what tools reach and report edits"}
}

// RefreshMachine rewrites an installed Pi extension with this build's template, and the
// terma it runs where the one it names is gone.
func (Agent) RefreshMachine() (string, bool, error) { return refreshExtension() }

var (
	_ agents.Covered          = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.MachineRefresher = Agent{}
	_ agents.Surfaced         = Agent{}
	_ agents.VersionReporter  = Agent{}
)
