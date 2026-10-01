package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// --- Cursor project hooks -------------------------------------------------------------

// hooksPath is Cursor's project-scope hooks file. It applies to the IDE and the
// cursor-agent CLI alike, Cursor watches it for changes, and it is meant to be committed.
const hooksPath = ".cursor/hooks.json"

// cursorHooksVersion is the schema version Cursor requires at the top of the file.
const cursorHooksVersion = "1"

// committedHooks are the adapter shims for Cursor. Each is a one-liner that forwards the
// hook's JSON to the binary; no logic lives here. afterFileEdit is the one Cursor event
// that names the file the agent changed; sessionStart and sessionEnd bracket the
// conversation. Additional hooks capture ordered turn observations, and the generic
// postToolUse pair reports every tool call. Terma emits no hook response: it never
// blocks prompts, adds followups or changes agent behavior — which is also why none of
// Cursor's permission gates (preToolUse, beforeShellExecution, beforeMCPExecution,
// beforeReadFile, subagentStart) is ever committed.
var committedHooks = []struct {
	Event   string
	Command string
}{
	{"sessionStart", hookmgr.HookCommand("cursor-session-start")},
	{"sessionEnd", hookmgr.HookCommand("cursor-session-end")},
	{"afterFileEdit", hookmgr.HookCommand("cursor-file-edit")},
	// postToolUse and postToolUseFailure fire for every tool type with a tool_use_id
	// and a duration; the shell- and MCP-specific after* hooks restate the same calls
	// without an id and with their output, so they stay unwired (hookrun.CursorPostToolUse).
	{"postToolUse", hookmgr.HookCommand("cursor-post-tool-use")},
	{"postToolUseFailure", hookmgr.HookCommand("cursor-post-tool-use-failure")},
	{"beforeSubmitPrompt", hookmgr.HookCommand("cursor-before-submit-prompt")},
	{"afterAgentResponse", hookmgr.HookCommand("cursor-after-agent-response")},
	{"stop", hookmgr.HookCommand("cursor-stop")},
	{"preCompact", hookmgr.HookCommand("cursor-pre-compact")},
	// subagentStop reports a finished subagent's outcome and the files it modified.
	// subagentStart is a permission gate — no output denies the spawn — so a committed
	// entry would block subagents on every machine without terma. It stays unwired.
	{"subagentStop", hookmgr.HookCommand("cursor-subagent-stop")},
}

// hasConfig reports whether the repository already carries Cursor configuration — a
// .cursor directory — which is when wiring its hooks by default is a help rather than a
// stray directory in a repository nobody opens in Cursor.
func hasConfig(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".cursor"))
	return err == nil && info.IsDir()
}

// planHooks merges terma's hooks into .cursor/hooks.json without disturbing
// anything else in the file (unknown keys survive byte-for-byte).
func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return planCursor(root, hooksPath, hookmgr.HookCommand, install)
}

// planUserHooks merges terma's hooks into Cursor's user-level hooks file
// (<dir>/hooks.json, ~/.cursor), with command naming each event: global mode's
// machine-wide hooks.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planCursor(dir, "hooks.json", command, install)
}

func planCursor(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookEntry struct {
		Command   string          `json:"command"`
		Timeout   int             `json:"timeout,omitempty"`
		LoopLimit json.RawMessage `json:"loop_limit,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(committedHooks))
	for _, h := range committedHooks {
		value := hookEntry{Command: command(hookmgr.HookEventOf(h.Command)), Timeout: 10}
		if h.Event == "stop" || h.Event == "subagentStop" {
			// Cursor defaults to skipping stop and subagentStop hooks after five
			// continuation loops. Observation must continue; terma never requests a loop.
			value.LoopLimit = json.RawMessage("null")
		}
		entry, err := hookmgr.MarshalJSON(value, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, hookmgr.EventHook{Event: h.Event, Entry: entry})
	}
	// Cursor refuses a file without its schema version, so terma sets one on a file it
	// creates. The version survives uninstall because it may predate Terma.
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{
		Path:     path,
		Defaults: map[string]json.RawMessage{"version": json.RawMessage(cursorHooksVersion)},
	}, own, install)
}
