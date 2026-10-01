package claude

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// settingsPath is Claude Code's project-scope settings file, which supports
// hooks and is meant to be committed.
const settingsPath = ".claude/settings.json"

// committedHooks are the adapter shims for Claude Code. Each is a one-liner that
// forwards the hook's JSON to the binary; no logic lives here.
var committedHooks = []struct {
	Event   string
	Matcher string
	Command string
}{
	{"SessionStart", "", hookmgr.HookCommand("session-start")},
	{"SessionEnd", "", hookmgr.HookCommand("session-end")},
	// The edit tools are what a manifest is built from. Agent (Task, in older builds) is
	// the call a subagent is launched by: its response is the one place the subagent's
	// model is named, and how the run went when the parent waited for it.
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Agent|Task", hookmgr.HookCommand("post-tool-use")},
	// Stop fires when the assistant has finished a turn and the person is reading:
	// refresh account evidence and start a background spool flush.
	{"Stop", "", hookmgr.HookCommand("stop")},
	{"StopFailure", "", hookmgr.HookCommand("stop-failure")},
	// UserPromptSubmit opens every turn: the one hook that runs before a turn exports
	// anything, so the local relay is up (and the session claimed) before the turn's
	// telemetry leaves the agent, even if the relay died since the last turn. It
	// prints nothing: Claude Code hands this hook's stdout to the model.
	{"UserPromptSubmit", "", hookmgr.HookCommand("user-prompt-submit")},
	// A subagent runs inside the session: the payload keeps session_id and adds
	// agent_id / agent_type. Both are notification-only for terma.
	{"SubagentStart", "", hookmgr.HookCommand("subagent-start")},
	{"SubagentStop", "", hookmgr.HookCommand("subagent-stop")},
}

// planSettings merges terma's hooks into .claude/settings.json without
// disturbing anything else in the file (unknown keys survive byte-for-byte).
func planSettings(root string, install bool) (hookmgr.Plan, error) {
	return planClaude(root, settingsPath, hookmgr.HookCommand, install)
}

// planUserHooks merges terma's hooks into Claude Code's user settings
// (<configDir>/settings.json), with command naming each event: global mode's
// machine-wide hooks (UserHookCommand).
func planUserHooks(configDir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planClaude(configDir, "settings.json", command, install)
}

func planClaude(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	type hookEntry struct {
		Matcher string            `json:"matcher,omitempty"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	own := make([]hookmgr.EventHook, 0, len(committedHooks))
	for _, h := range committedHooks {
		cmd, err := hookmgr.MarshalJSON(hookCmd{Type: "command", Command: command(hookmgr.HookEventOf(h.Command)), Timeout: 10}, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		entry, err := hookmgr.MarshalJSON(hookEntry{Matcher: h.Matcher, Hooks: []json.RawMessage{cmd}}, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, hookmgr.EventHook{Event: h.Event, Entry: entry})
	}
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{Path: path}, own, install)
}

// managedSettings is a managed-settings.json holding terma's hooks.
func managedSettings(command func(event string) string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "terma-managed")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	plan, err := planClaude(dir, "managed-settings.json", command, true)
	if err != nil {
		return nil, err
	}
	if err := hookmgr.Apply(dir, plan); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "managed-settings.json"))
}
