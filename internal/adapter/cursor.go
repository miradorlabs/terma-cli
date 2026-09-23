package adapter

import (
	"context"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"os/exec"
)

// cursor is Cursor's IDE and CLI: project hooks in .cursor/hooks.json, wired by default
// where the repository already carries a .cursor directory.
type cursor struct{}

func (cursor) Name() string        { return "cursor" }
func (cursor) DisplayName() string { return "Cursor" }

// Installed looks for either of Cursor's binaries: the agent CLI, or the editor's launcher.
func (cursor) Installed(context.Context) bool {
	for _, name := range []string{"cursor-agent", "cursor"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}
func (cursor) HooksPath() string        { return hookmgr.CursorHooksPath }
func (cursor) Default(root string) bool { return hookmgr.HasCursor(root) }
func (cursor) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCursorHooks(root, install)
}

func (cursor) Events() map[string]Handler {
	return map[string]Handler{
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

func (cursor) FlushAfter() []string {
	return []string{"cursor-session-end", "cursor-after-agent-response", "cursor-stop"}
}
