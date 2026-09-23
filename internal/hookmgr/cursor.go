package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// --- Cursor project hooks -------------------------------------------------------------

// CursorHooksPath is Cursor's project-scope hooks file. It applies to the IDE and the
// cursor-agent CLI alike, Cursor watches it for changes, and it is meant to be committed.
const CursorHooksPath = ".cursor/hooks.json"

// cursorHooksVersion is the schema version Cursor requires at the top of the file.
const cursorHooksVersion = "1"

// CursorHooks are the adapter shims for Cursor. Each is a one-liner that forwards the
// hook's JSON to the binary; no logic lives here. afterFileEdit is the one Cursor event
// that names the file the agent changed; sessionStart and sessionEnd bracket the
// conversation. Additional hooks capture ordered turn observations, and the generic
// postToolUse pair reports every tool call. Terma emits no hook response: it never
// blocks prompts, adds followups or changes agent behavior — which is also why none of
// Cursor's permission gates (preToolUse, beforeShellExecution, beforeMCPExecution,
// beforeReadFile, subagentStart) is ever committed.
var CursorHooks = []struct {
	Event   string
	Command string
}{
	{"sessionStart", HookCommand("cursor-session-start")},
	{"sessionEnd", HookCommand("cursor-session-end")},
	{"afterFileEdit", HookCommand("cursor-file-edit")},
	// postToolUse and postToolUseFailure fire for every tool type with a tool_use_id
	// and a duration; the shell- and MCP-specific after* hooks restate the same calls
	// without an id and with their output, so they stay unwired (hookrun.CursorPostToolUse).
	{"postToolUse", HookCommand("cursor-post-tool-use")},
	{"postToolUseFailure", HookCommand("cursor-post-tool-use-failure")},
	{"beforeSubmitPrompt", HookCommand("cursor-before-submit-prompt")},
	{"afterAgentResponse", HookCommand("cursor-after-agent-response")},
	{"stop", HookCommand("cursor-stop")},
	{"preCompact", HookCommand("cursor-pre-compact")},
	// subagentStop reports a finished subagent's outcome and the files it modified.
	// subagentStart is a permission gate — no output denies the spawn — so a committed
	// entry would block subagents on every machine without terma. It stays unwired.
	{"subagentStop", HookCommand("cursor-subagent-stop")},
}

// HasCursor reports whether the repository already carries Cursor configuration — a
// .cursor directory — which is when wiring its hooks by default is a help rather than a
// stray directory in a repository nobody opens in Cursor.
func HasCursor(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".cursor"))
	return err == nil && info.IsDir()
}

// PlanCursorHooks merges terma's hooks into .cursor/hooks.json without disturbing
// anything else in the file (unknown keys survive byte-for-byte).
func PlanCursorHooks(root string, install bool) (Plan, error) {
	type hookEntry struct {
		Command   string          `json:"command"`
		Timeout   int             `json:"timeout,omitempty"`
		LoopLimit json.RawMessage `json:"loop_limit,omitempty"`
	}
	own := make([]eventHook, 0, len(CursorHooks))
	for _, h := range CursorHooks {
		value := hookEntry{Command: h.Command, Timeout: 10}
		if h.Event == "stop" || h.Event == "subagentStop" {
			// Cursor defaults to skipping stop and subagentStop hooks after five
			// continuation loops. Observation must continue; terma never requests a loop.
			value.LoopLimit = json.RawMessage("null")
		}
		entry, err := marshalJSON(value, "", "")
		if err != nil {
			return Plan{}, err
		}
		own = append(own, eventHook{Event: h.Event, Entry: entry})
	}
	// Cursor refuses a file without its schema version, so terma sets one on a file it
	// creates and removes a file left holding nothing else.
	return mergeEventHooks(root, hooksFile{
		Path:     CursorHooksPath,
		Defaults: map[string]json.RawMessage{"version": json.RawMessage(cursorHooksVersion)},
	}, own, install)
}
