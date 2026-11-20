// Package antigravity integrates Antigravity CLI (agy).
package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Google's Antigravity CLI (`agy`), with machine-wide hooks in agy's global
// customization root, ~/.gemini/config/hooks.json, and its status line in agy's settings;
// ConfigDir is terma's, where setup records what it replaced there.
type Agent struct{ ConfigDir string }

func (Agent) Name() string        { return "antigravity" }
func (Agent) DisplayName() string { return "Antigravity" }
func (Agent) Installed(ctx context.Context) bool {
	return detect(ctx).Found
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"antigravity-pre-invocation":  preInvocation,
		"antigravity-post-tool-use":   postToolUse,
		"antigravity-post-invocation": postInvocation,
		"antigravity-stop":            stop,
	}
}

func (Agent) FlushAfter() []string { return []string{"antigravity-stop"} }

// Renders is the wrapped statusLine command; the renderer has its own deadline.
func (Agent) Renders() map[string]agents.RenderHandler {
	return map[string]agents.RenderHandler{antigravityStatusLineEvent: func(ctx context.Context, env hookrun.Env) int {
		renderer, err := statusLineRenderer(env.ConfigDir)
		if err != nil {
			env.Logf("status line record: %v", err)
		}
		return antigravityStatusLine(ctx, env, renderer, "")
	}}
}

// PayloadSession reads agy's protojson payload; the directory comes from it, since the
// hook runs in the folder holding hooks.json.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in antigravityHookInput
	if json.Unmarshal(payload, &in) != nil || in.ConversationID == "" {
		return hookrun.PayloadSession{}, false
	}
	return hookrun.PayloadSession{ID: in.ConversationID, Cwd: in.cwd("")}, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "hooks and its transcript, no OTLP export: each model call's tokens (thinking not counted apart), every turn and tool step; plan, quota and account from the status line, interactive sessions only; no cost or tool durations"}
}

var (
	_ agents.Covered          = Agent{}
	_ agents.StateKeeper      = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.PayloadReader    = Agent{}
	_ agents.UserHooks        = Agent{}
	_ agents.StatusLiner      = Agent{}
	_ agents.Renderer         = Agent{}
	_ agents.MachineRefresher = Agent{}
)

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string {
	return []string{antigravityObservationDir, antigravityTurnDir, antigravityCallDir, antigravityStatusLineDir}
}
