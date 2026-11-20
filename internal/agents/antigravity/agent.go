// Package antigravity integrates Antigravity CLI (agy).
package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Google's Antigravity CLI (`agy`), with machine-wide hooks in agy's global
// customization root, ~/.gemini/config/hooks.json.
type Agent struct{}

func (Agent) Name() string        { return "antigravity" }
func (Agent) DisplayName() string { return "Antigravity" }

// Surfaces is Antigravity as one choice, marked beta at setup and naming the first agy
// terma sets up.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: a.Name(), DisplayName: a.DisplayName() + " " + minimumVersion + "+ (beta)", Installed: a.Installed}}
}

// Installed reports whether an agy terma can set up is on the PATH: one before
// minimumVersion is not, so setup --yes leaves it out rather than stopping at its refusal.
func (Agent) Installed(ctx context.Context) bool {
	if _, old := tooOld(ctx); old {
		return false
	}
	return detect(ctx).Found
}

// DetectedVersion is the version of the agy on PATH, for doctor; "" when it cannot tell.
func (Agent) DetectedVersion(ctx context.Context) string { return detect(ctx).Version }

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"antigravity-pre-invocation":  preInvocation,
		"antigravity-post-tool-use":   postToolUse,
		"antigravity-post-invocation": postInvocation,
		"antigravity-stop":            stop,
	}
}

func (Agent) FlushAfter() []string { return []string{"antigravity-stop"} }

// WhenHooksOff still answers each hook with the {} agy expects, with hooks switched off or
// terma torn down while a session runs.
func (a Agent) WhenHooksOff() map[string]agents.Handler {
	off := map[string]agents.Handler{}
	for event := range a.Events() {
		off[event] = func(_ context.Context, env hookrun.Env) error {
			antigravityAck(env)
			return nil
		}
	}
	return off
}

// PayloadSession reads agy's protojson payload; the directory comes from it, since the
// hook runs in the folder holding hooks.json.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in antigravityHookInput
	if json.Unmarshal(payload, &in) != nil || in.id() == "" {
		return hookrun.PayloadSession{}, false
	}
	return hookrun.PayloadSession{ID: in.id(), Cwd: in.cwd("")}, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportPartial, Note: "files its file tools write; a file a shell command writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "hooks only: model, turns, every tool step and termination; no token counts, plan, quota, cost or tool durations — agy has no OTLP export"}
}

var (
	_ agents.Covered          = Agent{}
	_ agents.StateKeeper      = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.PayloadReader    = Agent{}
	_ agents.UserHooks        = Agent{}
	_ agents.Surfaced         = Agent{}
	_ agents.OffSwitched      = Agent{}
	_ agents.VersionReporter  = Agent{}
	_ agents.MachineRefresher = Agent{}
)

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string { return []string{antigravityObservationDir, antigravityTurnDir, antigravityCallDir} }
