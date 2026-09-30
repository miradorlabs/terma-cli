// Package antigravity integrates Antigravity CLI (agy).
package antigravity

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Google's Antigravity CLI (`agy`), the successor to Gemini CLI. Its
// hooks live in .agents/hooks.json, the customization root agy shares with its rules
// and skills, and are wired by default where the repository already carries one of
// agy's customization directories.
type Agent struct{}

func (Agent) Name() string        { return "antigravity" }
func (Agent) DisplayName() string { return "Antigravity" }
func (Agent) Installed(ctx context.Context) bool {
	return harness.Antigravity{}.Detect(ctx).Found
}
func (Agent) HooksPath() string        { return hookmgr.AntigravityHooksPath }
func (Agent) Default(root string) bool { return hookmgr.HasAntigravity(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanAntigravityHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"antigravity-pre-invocation":  hookrun.AntigravityPreInvocation,
		"antigravity-post-tool-use":   hookrun.AntigravityPostToolUse,
		"antigravity-post-invocation": hookrun.AntigravityPostInvocation,
		"antigravity-stop":            hookrun.AntigravityStop,
	}
}

func (Agent) FlushAfter() []string { return []string{"antigravity-stop"} }

// Trust answers two questions agy never raises itself. Hooks load only for a workspace
// the developer has trusted from inside agy (the record is agy's own settings file), and
// terma's named entry in .agents/hooks.json can be switched off with `"enabled": false`
// — a switch `terma install` deliberately preserves. Either way the committed file is
// inert and nothing says so.
func (a Agent) Trust(root string) (agents.TrustState, error) {
	if !hookmgr.AntigravityHooksEnabled(root) {
		return agents.TrustState{
			Detail: `, but terma's entry is switched off ("enabled": false), so agy runs none of them`,
			Fix:    "remove \"enabled\": false from the terma entry in " + a.HooksPath(),
		}, nil
	}
	trusted, err := (harness.Antigravity{}).TrustsWorkspace(root)
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

var (
	_ agents.Agent    = Agent{}
	_ agents.Trusting = Agent{}
)
