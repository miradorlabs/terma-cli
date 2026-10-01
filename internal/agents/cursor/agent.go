// Package cursor integrates Cursor.
package cursor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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
func (Agent) HooksPath() string        { return hooksPath }
func (Agent) Default(root string) bool { return hasConfig(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"cursor-session-start":         sessionStart,
		"cursor-session-end":           sessionEnd,
		"cursor-file-edit":             fileEdit,
		"cursor-post-tool-use":         postToolUse,
		"cursor-post-tool-use-failure": postToolUseFailure,
		"cursor-before-submit-prompt":  beforeSubmitPrompt,
		"cursor-after-agent-response":  afterAgentResponse,
		"cursor-stop":                  stop,
		"cursor-pre-compact":           preCompact,
		"cursor-subagent-stop":         subagentStop,
	}
}

func (Agent) FlushAfter() []string {
	return []string{"cursor-session-end", "cursor-after-agent-response", "cursor-stop"}
}

func (Agent) UserHooksPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "hooks.json"), err
}
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}

// PayloadSession reads Cursor's payload, keyed on conversation_id: the one id every
// Cursor event carries.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in struct {
		ConversationID string `json:"conversation_id"`
		Cwd            string `json:"cwd"`
	}
	if json.Unmarshal(payload, &in) != nil || in.ConversationID == "" {
		return hookrun.PayloadSession{}, false
	}
	return hookrun.PayloadSession{ID: in.ConversationID, Cwd: in.Cwd}, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "ordered hook observations, every tool call and optional token snapshots; plan, quota and billed cost unavailable; backend mapping of tool calls pending"}
}

var (
	_ agents.Covered       = Agent{}
	_ agents.PayloadReader = Agent{}
	_ agents.Agent         = Agent{}
	_ agents.UserHooks     = Agent{}
)
