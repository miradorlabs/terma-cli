// Package claude integrates Claude Code, and Claude Desktop through the same settings.
package claude

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Claude Code: project hooks in .claude/settings.json, wired into every
// repository by default because the file is Claude Code's own project settings and a
// repository without one loses nothing by gaining it.
type Agent struct{}

func (Agent) Name() string                       { return "claude" }
func (Agent) DisplayName() string                { return "Claude Code" }
func (Agent) Installed(ctx context.Context) bool { return harness.Claude{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hookmgr.ClaudeSettingsPath }
func (Agent) Default(string) bool                { return true }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanClaudeSettings(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"session-start": hookrun.SessionStart,
		"session-end":   hookrun.SessionEnd,
		"post-tool-use": hookrun.PostToolUse,
		"stop":          hookrun.Stop,
		"stop-failure":  hookrun.StopFailure,
		// Turn start: claims the session for the local relay and starts it (cmd/hook.go
		// does both from the payload); the handler itself only reads the payload.
		"user-prompt-submit": hookrun.UserPromptSubmit,
		// A subagent runs inside the session; both are notification-only for terma.
		"subagent-start": hookrun.SubagentStart,
		"subagent-stop":  hookrun.SubagentStop,
	}
}

func (Agent) FlushAfter() []string { return []string{"session-end", "stop", "stop-failure"} }

func (Agent) UserHooksPath() (string, error) { return (harness.Claude{}).ConfigPath() }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanClaudeUserHooks(dir, command, install)
}
func (Agent) ManagedHookFiles(root string) []string { return harness.ClaudeManagedHookFiles(root) }

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return harness.Claude{} }

var (
	_ agents.Exporting    = Agent{}
	_ agents.Agent        = Agent{}
	_ agents.UserHooks    = Agent{}
	_ agents.ManagedHooks = Agent{}
)
