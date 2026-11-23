// Package grok integrates xAI's grok CLI (Grok Build): its native OTLP log export, pointed at
// the local relay through [telemetry] in grok's config.toml (harness.go), and terma's own
// hooks file in grok's hooks directory, which claims each session for its repository, carries
// the prompt (prompt.go), names a subagent's parent (subagent.go) and reports file edits
// (handlers.go).
package grok

import (
	"context"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "grok"
	displayName = "Grok Build"

	// hookEventEnv is set on every hook process grok starts, and on those alone: the commands
	// its shell tool runs get GROK_SESSION_ID but not this (grok 1.0.92).
	hookEventEnv = "GROK_HOOK_EVENT"
)

// The hook events terma's hooks file runs, each grok-<event>.
const (
	eventSessionStart     = "grok-session-start"
	eventUserPromptSubmit = "grok-user-prompt-submit"
	eventPostToolUse      = "grok-post-tool-use"
	eventSubagentStart    = "grok-subagent-start"
	eventStop             = "grok-stop"
	eventSessionEnd       = "grok-session-end"
)

// Agent is grok: telemetry from its native log export, attribution and prompts from terma's
// hooks.
type Agent struct {
	// ConfigDir is terma's config directory, which holds the journal of grok's config.toml.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

// Installed reports whether a grok terma can set up is on the PATH: one before minimumVersion
// is not, so setup --yes leaves it out rather than stopping at its refusal.
func (Agent) Installed(ctx context.Context) bool {
	if _, old := tooOld(ctx); old {
		return false
	}
	return exporter{}.Detect(ctx).Found
}

// DetectedVersion is the version of the grok on PATH, for doctor; "" when it cannot tell.
func (Agent) DetectedVersion(ctx context.Context) string { return exporter{}.Detect(ctx).Version }

// Surfaces is grok as one choice, marked beta at setup.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: name, DisplayName: displayName + " (beta)", Installed: a.Installed}}
}

// Events are the grok-* hooks terma's hooks file runs.
func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		eventSessionStart:     sessionStart,
		eventUserPromptSubmit: userPromptSubmit,
		eventPostToolUse:      postToolUse,
		eventSubagentStart:    subagentStart,
		eventStop:             stop,
		eventSessionEnd:       sessionEnd,
	}
}

// FlushAfter delivers the spool at each turn's end and when a session ends.
func (Agent) FlushAfter() []string { return []string{eventStop, eventSessionEnd} }

// StartedHook reports whether grok started this hook process: grok also runs the entries in
// Claude Code's and Cursor's hooks files, which then stand down.
func (Agent) StartedHook(getenv func(string) string) bool { return getenv(hookEventEnv) != "" }

// Harness is how terma configures grok's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportPartial, Note: "files its search_replace tool edits; a file a shell command or the write tool writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "model calls with tokens and grok's own cost, replies, tool calls and compaction ride grok's native OTLP logs to the local relay; terma's hooks report sessions, prompts, subagent parents and edits. A sandboxed session is not collected"}
}

// ContentConsented is the consent a prompt's text travels under: grok among the developer's
// agents, or global mode.
func (Agent) ContentConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}

var (
	_ agents.Agent           = Agent{}
	_ agents.Covered         = Agent{}
	_ agents.Exporting       = Agent{}
	_ agents.Surfaced        = Agent{}
	_ agents.UserHooks       = Agent{}
	_ agents.HookStarter     = Agent{}
	_ agents.ContentConsent  = Agent{}
	_ agents.VersionReporter = Agent{}
)
