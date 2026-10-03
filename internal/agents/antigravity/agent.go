// Package antigravity integrates Antigravity CLI (agy).
package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Google's Antigravity CLI (`agy`), with hooks in .agents/hooks.json, wired by
// default where the repository already carries one of agy's customization directories.
type Agent struct{}

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

// PayloadSession reads agy's protojson payload; the workspace comes from it, since the
// hook runs in <repo>/.agents.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in struct {
		ConversationID string   `json:"conversationId"`
		WorkspacePaths []string `json:"workspacePaths"`
	}
	if json.Unmarshal(payload, &in) != nil || in.ConversationID == "" {
		return hookrun.PayloadSession{}, false
	}
	s := hookrun.PayloadSession{ID: in.ConversationID}
	if len(in.WorkspacePaths) > 0 {
		s.Cwd = in.WorkspacePaths[0]
	}
	return s, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "hooks only: model, turns, every tool step and termination; no token counts, plan, quota, cost or tool durations — agy has no OTLP export; backend mapping of tool calls pending"}
}

var (
	_ agents.Covered       = Agent{}
	_ agents.Agent         = Agent{}
	_ agents.PayloadReader = Agent{}
)
