// Package antigravity integrates Antigravity CLI (agy).
package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
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
func (Agent) HooksPath() string        { return hooksPath }
func (Agent) Default(root string) bool { return hasConfig(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
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

// Trust reports the two ways agy silently skips terma's hooks: an untrusted workspace, or
// terma's entry switched off with `"enabled": false`, which install preserves.
func (a Agent) Trust(root string) (agents.TrustState, error) {
	if !hooksEnabled(root) {
		return agents.TrustState{
			Detail: `, but terma's entry is switched off ("enabled": false), so agy runs none of them`,
			Fix:    "remove \"enabled\": false from the terma entry in " + a.HooksPath(),
		}, nil
	}
	trusted, err := trustsWorkspace(root)
	if err != nil {
		return agents.TrustState{}, err
	}
	if !trusted {
		return agents.TrustState{
			Detail: ", but this repository is not a trusted Antigravity workspace, so agy runs none of them",
			Fix:    "open agy in this repository and trust the workspace when asked",
		}, nil
	}
	return agents.TrustState{Trusted: true, Detail: " and the workspace is trusted"}, nil
}

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
	_ agents.Trusting      = Agent{}
)
