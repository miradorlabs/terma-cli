package adapter

import (
	"context"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// claude is Claude Code: project hooks in .claude/settings.json, wired into every
// repository by default because the file is Claude Code's own project settings and a
// repository without one loses nothing by gaining it.
type claude struct{}

func (claude) Name() string                       { return "claude" }
func (claude) DisplayName() string                { return "Claude Code" }
func (claude) Installed(ctx context.Context) bool { return harness.Claude{}.Detect(ctx).Found }
func (claude) HooksPath() string                  { return hookmgr.ClaudeSettingsPath }
func (claude) Default(string) bool                { return true }
func (claude) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanClaudeSettings(root, install)
}

func (claude) Events() map[string]Handler {
	return map[string]Handler{
		"session-start": hookrun.SessionStart,
		"session-end":   hookrun.SessionEnd,
		"post-tool-use": hookrun.PostToolUse,
		"stop":          hookrun.Stop,
		"stop-failure":  hookrun.StopFailure,
		// A subagent runs inside the session; both are notification-only for terma.
		"subagent-start": hookrun.SubagentStart,
		"subagent-stop":  hookrun.SubagentStop,
	}
}

func (claude) FlushAfter() []string { return []string{"session-end", "stop", "stop-failure"} }
