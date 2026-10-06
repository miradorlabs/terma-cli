package claude

import (
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// claudeHooks are the hooks that forward each event's JSON to the binary.
var claudeHooks = []struct {
	Event   string
	Matcher string
	Hook    string
}{
	{"SessionStart", "", "session-start"},
	{"SessionEnd", "", "session-end"},
	// Edits build the manifest; the Agent tool's response (Task in older builds) is the only place a
	// subagent's model is named.
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Agent|Task", "post-tool-use"},
	{"Stop", "", "stop"},
	{"StopFailure", "", "stop-failure"},
	// The one hook before a turn exports anything, so the relay is up and the session claimed first.
	// It must print nothing: its stdout goes to the model.
	{"UserPromptSubmit", "", "user-prompt-submit"},
	{"SubagentStart", "", "subagent-start"},
	{"SubagentStop", "", "subagent-stop"},
}

// planUserHooks merges terma's machine-wide hooks into <configDir>/settings.json; unknown
// keys survive byte-for-byte.
func planUserHooks(configDir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planClaude(configDir, "settings.json", command, install)
}

func planClaude(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(claudeHooks))
	for _, h := range claudeHooks {
		entry, err := hookmgr.Group(h.Event, h.Matcher, hookCmd{Type: "command", Command: command(h.Hook), Timeout: 10})
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
