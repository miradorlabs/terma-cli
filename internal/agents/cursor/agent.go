// Package cursor integrates Cursor.
package cursor

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Cursor's IDE and CLI: project hooks in .cursor/hooks.json, wired by default
// where the repository already carries a .cursor directory.
type Agent struct{}

func (Agent) Name() string        { return "cursor" }
func (Agent) DisplayName() string { return "Cursor" }

// Installed looks for either of Cursor's binaries: the agent CLI, or the editor's launcher.
func (Agent) Installed(context.Context) bool {
	for _, name := range []string{"cursor-agent", "cursor"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}
func (Agent) HooksPath() string        { return hookmgr.CursorHooksPath }
func (Agent) Default(root string) bool { return hookmgr.HasCursor(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCursorHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"cursor-session-start":         hookrun.CursorSessionStart,
		"cursor-session-end":           hookrun.CursorSessionEnd,
		"cursor-file-edit":             hookrun.CursorFileEdit,
		"cursor-post-tool-use":         hookrun.CursorPostToolUse,
		"cursor-post-tool-use-failure": hookrun.CursorPostToolUseFailure,
		"cursor-before-submit-prompt":  hookrun.CursorBeforeSubmitPrompt,
		"cursor-after-agent-response":  hookrun.CursorAfterAgentResponse,
		"cursor-stop":                  hookrun.CursorStop,
		"cursor-pre-compact":           hookrun.CursorPreCompact,
		"cursor-subagent-stop":         hookrun.CursorSubagentStop,
	}
}

func (Agent) FlushAfter() []string {
	return []string{"cursor-session-end", "cursor-after-agent-response", "cursor-stop"}
}

func (Agent) UserHooksPath() (string, error) { return harness.CursorUserHooksPath() }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCursorUserHooks(dir, command, install)
}

var (
	_ agents.Agent     = Agent{}
	_ agents.UserHooks = Agent{}
)
