package adapter

import (
	"context"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// antigravity is Google's Antigravity CLI (`agy`), the successor to Gemini CLI. Its
// hooks live in .agents/hooks.json, the customization root agy shares with its rules
// and skills, and are wired by default where the repository already carries one of
// agy's customization directories.
type antigravity struct{}

func (antigravity) Name() string        { return "antigravity" }
func (antigravity) DisplayName() string { return "Antigravity" }
func (antigravity) Installed(ctx context.Context) bool {
	return harness.Antigravity{}.Detect(ctx).Found
}
func (antigravity) HooksPath() string        { return hookmgr.AntigravityHooksPath }
func (antigravity) Default(root string) bool { return hookmgr.HasAntigravity(root) }
func (antigravity) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanAntigravityHooks(root, install)
}

func (antigravity) Events() map[string]Handler {
	return map[string]Handler{
		"antigravity-pre-invocation":  hookrun.AntigravityPreInvocation,
		"antigravity-post-tool-use":   hookrun.AntigravityPostToolUse,
		"antigravity-post-invocation": hookrun.AntigravityPostInvocation,
		"antigravity-stop":            hookrun.AntigravityStop,
	}
}

func (antigravity) FlushAfter() []string { return []string{"antigravity-stop"} }

// Trust answers two questions agy never raises itself. Hooks load only for a workspace
// the developer has trusted from inside agy (the record is agy's own settings file), and
// terma's named entry in .agents/hooks.json can be switched off with `"enabled": false`
// — a switch `terma install` deliberately preserves. Either way the committed file is
// inert and nothing says so.
func (a antigravity) Trust(root string) (TrustState, error) {
	if !hookmgr.AntigravityHooksEnabled(root) {
		return TrustState{
			Detail: `, but terma's entry is switched off ("enabled": false), so agy runs none of them`,
			Fix:    "remove \"enabled\": false from the terma entry in " + a.HooksPath(),
		}, nil
	}
	trusted, err := (harness.Antigravity{}).TrustsWorkspace(root)
	if err != nil {
		return TrustState{}, err
	}
	if !trusted {
		return TrustState{
			Detail: ", but this repository is not a trusted Antigravity workspace, so agy runs none of them",
			Fix:    "open agy in this repository and trust the workspace when asked",
		}, nil
	}
	return TrustState{Trusted: true, Detail: " and the workspace is trusted"}, nil
}
