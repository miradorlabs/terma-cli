package claude

import (
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// settingsPath is the committed project settings file, which carries hooks.
const settingsPath = ".claude/settings.json"

// committedHooks are the one-line shims that forward each hook's JSON to the binary.
var committedHooks = []struct {
	Event   string
	Matcher string
	Command string
}{
	{"SessionStart", "", hookmgr.HookCommand("session-start")},
	{"SessionEnd", "", hookmgr.HookCommand("session-end")},
	// Edits build the manifest; the Agent tool's response (Task in older builds) is the only place a
	// subagent's model is named.
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Agent|Task", hookmgr.HookCommand("post-tool-use")},
	{"Stop", "", hookmgr.HookCommand("stop")},
	{"StopFailure", "", hookmgr.HookCommand("stop-failure")},
	// The one hook before a turn exports anything, so the relay is up and the session claimed first.
	// It must print nothing: its stdout goes to the model.
	{"UserPromptSubmit", "", hookmgr.HookCommand("user-prompt-submit")},
	{"SubagentStart", "", hookmgr.HookCommand("subagent-start")},
	{"SubagentStop", "", hookmgr.HookCommand("subagent-stop")},
}

// planSettings merges terma's hooks into .claude/settings.json; unknown keys survive byte-for-byte.
func planSettings(root string, install bool) (hookmgr.Plan, error) {
	return planClaude(root, settingsPath, hookmgr.HookCommand, install)
}

// planUserHooks merges global mode's machine-wide hooks into <configDir>/settings.json.
func planUserHooks(configDir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planClaude(configDir, "settings.json", command, install)
}

func planClaude(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(committedHooks))
	for _, h := range committedHooks {
		entry, err := hookmgr.Group(h.Event, h.Matcher, hookCmd{Type: "command", Command: command(hookmgr.HookEventOf(h.Command)), Timeout: 10})
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, entry)
	}
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{Path: path}, own, install)
}

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
