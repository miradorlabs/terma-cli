package hookmgr

import (
	"encoding/json"
)

// ClaudeSettingsPath is Claude Code's project-scope settings file, which supports
// hooks and is meant to be committed.
const ClaudeSettingsPath = ".claude/settings.json"

// ClaudeHooks are the adapter shims for Claude Code. Each is a one-liner that
// forwards the hook's JSON to the binary; no logic lives here.
var ClaudeHooks = []struct {
	Event   string
	Matcher string
	Command string
}{
	{"SessionStart", "", HookCommand("session-start")},
	{"SessionEnd", "", HookCommand("session-end")},
	// The edit tools are what a manifest is built from. Agent (Task, in older builds) is
	// the call a subagent is launched by: its response is the one place the subagent's
	// model is named, and how the run went when the parent waited for it.
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Agent|Task", HookCommand("post-tool-use")},
	// Stop fires when the assistant has finished a turn and the person is reading:
	// refresh account evidence and start a background spool flush.
	{"Stop", "", HookCommand("stop")},
	{"StopFailure", "", HookCommand("stop-failure")},
	// UserPromptSubmit opens every turn: the one hook that runs before a turn exports
	// anything, so the local relay is up (and the session claimed) before the turn's
	// telemetry leaves the agent, even if the relay died since the last turn. It
	// prints nothing: Claude Code hands this hook's stdout to the model.
	{"UserPromptSubmit", "", HookCommand("user-prompt-submit")},
	// A subagent runs inside the session: the payload keeps session_id and adds
	// agent_id / agent_type. Both are notification-only for terma.
	{"SubagentStart", "", HookCommand("subagent-start")},
	{"SubagentStop", "", HookCommand("subagent-stop")},
}

// PlanClaudeSettings merges terma's hooks into .claude/settings.json without
// disturbing anything else in the file (unknown keys survive byte-for-byte).
func PlanClaudeSettings(root string, install bool) (Plan, error) {
	return planClaude(root, ClaudeSettingsPath, HookCommand, install)
}

// PlanClaudeUserHooks merges terma's hooks into Claude Code's user settings
// (<configDir>/settings.json), with command naming each event: global mode's
// machine-wide hooks (UserHookCommand).
func PlanClaudeUserHooks(configDir string, command func(event string) string, install bool) (Plan, error) {
	return planClaude(configDir, "settings.json", command, install)
}

func planClaude(root, path string, command func(event string) string, install bool) (Plan, error) {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	type hookEntry struct {
		Matcher string            `json:"matcher,omitempty"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	own := make([]eventHook, 0, len(ClaudeHooks))
	for _, h := range ClaudeHooks {
		cmd, err := marshalJSON(hookCmd{Type: "command", Command: command(hookEventOf(h.Command)), Timeout: 10}, "", "")
		if err != nil {
			return Plan{}, err
		}
		entry, err := marshalJSON(hookEntry{Matcher: h.Matcher, Hooks: []json.RawMessage{cmd}}, "", "")
		if err != nil {
			return Plan{}, err
		}
		own = append(own, eventHook{Event: h.Event, Entry: entry})
	}
	return mergeEventHooks(root, hooksFile{Path: path}, own, install)
}
