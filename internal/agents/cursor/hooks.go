package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksPath is Cursor's project-scope hooks file, read by the IDE and cursor-agent alike.
const hooksPath = ".cursor/hooks.json"

const cursorHooksVersion = "1"

// committedHooks are terma's Cursor hooks. None of Cursor's permission gates (preToolUse,
// before*, subagentStart) is committed: terma never decides for an agent, and the guard's
// empty reply denies a subagent spawn on a machine without terma.
var committedHooks = []struct {
	Event   string
	Command string
}{
	{"sessionStart", hookmgr.HookCommand("cursor-session-start")},
	{"sessionEnd", hookmgr.HookCommand("cursor-session-end")},
	{"afterFileEdit", hookmgr.HookCommand("cursor-file-edit")},
	// The shell- and MCP-specific after* hooks restate these calls without an id.
	{"postToolUse", hookmgr.HookCommand("cursor-post-tool-use")},
	{"postToolUseFailure", hookmgr.HookCommand("cursor-post-tool-use-failure")},
	{"beforeSubmitPrompt", hookmgr.HookCommand("cursor-before-submit-prompt")},
	{"afterAgentResponse", hookmgr.HookCommand("cursor-after-agent-response")},
	{"stop", hookmgr.HookCommand("cursor-stop")},
	{"preCompact", hookmgr.HookCommand("cursor-pre-compact")},
	{"subagentStop", hookmgr.HookCommand("cursor-subagent-stop")},
}

// hasConfig reports whether the repository has a .cursor directory, so wiring its hooks
// by default adds no stray directory.
func hasConfig(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".cursor"))
	return err == nil && info.IsDir()
}

func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return planCursor(root, hooksPath, hookmgr.HookCommand, install)
}

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
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{
		Path:     path,
		Defaults: map[string]json.RawMessage{"version": json.RawMessage(cursorHooksVersion)},
	}, own, install)
}
