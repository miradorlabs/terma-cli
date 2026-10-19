package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksFile is terma's own file in Copilot's user-level hooks directory, which the CLI and
// VS Code both read every *.json of.
const hooksFile = "terma.json"

// hooksVersion is the schema version Copilot requires of a hooks file.
const hooksVersion = "1"

// copilotHooks are terma's Copilot hooks, by the PascalCase event names: under those the
// CLI and VS Code both send the snake_case payload (session_id, cwd, tool_name). No
// permission gate (PreToolUse) is written: terma never decides for an agent.
var copilotHooks = []struct {
	Event string
	Hook  string
}{
	{"SessionStart", "copilot-session-start"},
	{"UserPromptSubmit", "copilot-prompt"},
	{"PostToolUse", "copilot-post-tool-use"},
	{"Stop", "copilot-stop"},
	{"SessionEnd", "copilot-session-end"},
}

// copilotHome is where Copilot keeps its user-level files: COPILOT_HOME, else ~/.copilot.
func copilotHome() (string, error) {
	if d := os.Getenv("COPILOT_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".copilot"), err
}

// UserHooksPath is terma's file among Copilot's user-level hooks.
func (Agent) UserHooksPath() (string, error) {
	home, err := copilotHome()
	return filepath.Join(home, "hooks", hooksFile), err
}

// PlanUserHooks merges terma's machine-wide hooks into <dir>/terma.json.
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}

func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookEntry struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		// PowerShell is the Windows twin: neither Windows shell parses the POSIX line.
		PowerShell string `json:"powershell,omitempty"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	own := make([]hookmgr.EventHook, 0, len(copilotHooks))
	for _, h := range copilotHooks {
		value := hookEntry{Type: "command", Command: command(h.Hook), TimeoutSec: 10}
		if runtime.GOOS == "windows" {
			value.PowerShell, _ = hookmgr.WindowsHookCommand(hookmgr.SystemCmd(), value.Command)
		}
		entry, err := hookmgr.MarshalJSON(value, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, hookmgr.EventHook{Event: h.Event, Entry: entry})
	}
	defaults := map[string]json.RawMessage{"version": json.RawMessage(hooksVersion)}
	plan, err := hookmgr.MergeEventHooks(dir, hookmgr.HooksFile{Path: hooksFile, Defaults: defaults}, own, install)
	if err != nil || install {
		return plan, err
	}
	// The file is terma's own: one left holding only the version goes with its hooks.
	for i, c := range plan.Changes {
		var rest map[string]json.RawMessage
		if json.Unmarshal(c.After, &rest) == nil && len(rest) == 1 && string(rest["version"]) == hooksVersion {
			plan.Changes[i].After = nil
		}
	}
	return plan, nil
}

var _ agents.UserHooks = Agent{}
