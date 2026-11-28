// Package opencode integrates OpenCode through terma's plugin, which OpenCode loads from its
// plugins directory and which is the only record of an OpenCode session: it exports a span
// per model call and per tool call and a log record per prompt and per title to the local
// relay, and runs `terma hook opencode-<event>` to claim each session for its repository
// and report the files an edit changed.
// One plugin file serves OpenCode 1 and OpenCode 2 (plugin/terma.js); harness.go writes it
// and refreshes it, relay.go points it at the relay and says what keeps it from there,
// telemetry.go is what the relay withholds from its export, and minimum_version.go the
// first build of each major it works with.
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

// The hook events the plugin runs beyond the shared extension's.
const (
	eventPrompt     = "opencode-prompt"
	eventIdle       = "opencode-idle"
	eventSessionEnd = "opencode-session-end"
)

// Agent is OpenCode, whose events come from terma's user-scope plugin; it has no
// repository-scope hooks.
type Agent struct {
	// ConfigDir is terma's config directory, which holds the helper scripts.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

// Installed reports whether an OpenCode terma can set up is on the PATH: one older than the
// minimum for its major is not, so setup --yes leaves it out rather than stopping at its
// refusal.
func (Agent) Installed(ctx context.Context) bool {
	if _, old := tooOld(ctx); old {
		return false
	}
	return exporter{}.Detect(ctx).Found
}

// DetectedVersion is the version of the OpenCode on PATH, for doctor; "" when it cannot tell.
func (Agent) DetectedVersion(ctx context.Context) string { return exporter{}.Detect(ctx).Version }

// Surfaces is OpenCode as one choice, marked beta at setup.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: displayName + " (beta)", Installed: a.Installed}}
}

// Events are the opencode-* hooks the plugin runs: the shared extension's, and the end of a
// turn. The prompt and idle hooks record nothing themselves: the dispatch claims the session
// from their payload once the handler returns, so a session that only chats keeps its
// claim, and the idle hook's flush delivers what the turn spooled. The plugin names a
// subagent's parent on its own spans, so no claim needs to.
func (Agent) Events() map[string]agents.Handler {
	ext := hookrun.Extension{Tool: name}
	events := ext.Events(name)
	events[eventPrompt] = ext.VerifyPrompt
	events[eventIdle] = hookrun.TurnStart
	return events
}

// FlushAfter delivers the spool at each turn's end and when a session is deleted.
func (Agent) FlushAfter() []string { return []string{eventIdle, eventSessionEnd} }

// Harness is how terma configures the agent's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

// RefreshMachine rewrites the plugin from this build, and the terma it runs where the one it
// names is gone.
func (Agent) RefreshMachine() (string, bool, error) { return exporter{}.RefreshPlugin() }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull, Note: "files its write, edit and patch tools change; a file a shell command writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "model calls with tokens and OpenCode's own cost, replies, tool calls, MCP calls, subagents, prompts and titles ride terma's plugin to the local relay; its hooks claim sessions and report edits. On OpenCode 1 the call that writes a session's title has no tokens or cost"}
}

var (
	_ agents.Agent            = Agent{}
	_ agents.Covered          = Agent{}
	_ agents.MachineRefresher = Agent{}
	_ agents.Exporting        = Agent{}
	_ agents.Surfaced         = Agent{}
	_ agents.VersionReporter  = Agent{}
)
