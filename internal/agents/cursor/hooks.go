package cursor

import (
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

const cursorHooksVersion = "1"

// cursorHooks are terma's Cursor hooks. None of Cursor's permission gates (preToolUse,
// before*, subagentStart) is written: terma never decides for an agent, and the guard's
// empty reply denies a subagent spawn on a machine without terma.
var cursorHooks = []struct {
	Event string
	Hook  string
}{
	{"sessionStart", "cursor-session-start"},
	{"sessionEnd", "cursor-session-end"},
	{"afterFileEdit", "cursor-file-edit"},
	// The shell- and MCP-specific after* hooks restate these calls without an id.
	{"postToolUse", "cursor-post-tool-use"},
	{"postToolUseFailure", "cursor-post-tool-use-failure"},
	{"beforeSubmitPrompt", "cursor-before-submit-prompt"},
	{"afterAgentResponse", "cursor-after-agent-response"},
	{"stop", "cursor-stop"},
	{"preCompact", "cursor-pre-compact"},
	{"subagentStop", "cursor-subagent-stop"},
}

// planUserHooks merges terma's machine-wide hooks into <dir>/hooks.json.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookEntry struct {
		Command   string          `json:"command"`
		Timeout   int             `json:"timeout,omitempty"`
		LoopLimit json.RawMessage `json:"loop_limit,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(cursorHooks))
	for _, h := range cursorHooks {
		value := hookEntry{Command: command(h.Hook), Timeout: 10}
		if h.Event == "stop" || h.Event == "subagentStop" {
			// Cursor skips these after five continuation loops; observation must continue.
			value.LoopLimit = json.RawMessage("null")
		}
		entry, err := hookmgr.MarshalJSON(value, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, hookmgr.EventHook{Event: h.Event, Entry: entry})
	}
	// Cursor refuses a file without its schema version; it survives uninstall, as it may
	// predate terma.
	return hookmgr.MergeEventHooks(dir, hookmgr.HooksFile{
		Path:     "hooks.json",
		Defaults: map[string]json.RawMessage{"version": json.RawMessage(cursorHooksVersion)},
	}, own, install)
}
