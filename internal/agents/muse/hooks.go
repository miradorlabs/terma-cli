package muse

import (
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// museSettings is Muse's user-level settings file, under its config directory. It holds the
// developer's settings beside the hooks, and Muse rewrites it itself (a plugin's approval):
// terma's entries merge in and out, every other member kept as it was read.
const museSettings = "settings.json"

// museHooks are the events terma's hooks run on. Muse reads PascalCase event names and
// a string command; a hook must print nothing, since what UserPromptSubmit's prints reaches
// the model. PostLLMCall is asynchronous: it carries the call's whole conversation, and the
// model's loop never waits on terma for it.
var museHooks = []struct {
	Event string
	Hook  string
	Async bool
}{
	{"SessionStart", "muse-session-start", false},
	{"UserPromptSubmit", "muse-prompt", false},
	{"PostToolUse", "muse-post-tool-use", false},
	{"PostToolUseFailure", "muse-post-tool-use", false},
	{"PostLLMCall", "muse-post-llm-call", true},
	{"SubagentStart", "muse-subagent-start", false},
	{"SubagentStop", "muse-subagent-stop", false},
	{"Stop", "muse-stop", false},
	{"StopFailure", "muse-stop", false},
	{"SessionEnd", "muse-session-end", false},
}

// planHooks merges terma's hooks into <dir>/settings.json; a file terma creates opens with
// the schema version Muse requires of it.
func planHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
		Async   bool   `json:"async,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(museHooks))
	for _, h := range museHooks {
		entry, err := hookmgr.Group(h.Event, "", handler{Type: "command", Command: command(h.Hook), Timeout: 10, Async: h.Async})
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, entry)
	}
	return hookmgr.MergeEventHooks(dir, hookmgr.HooksFile{
		Path:     museSettings,
		Defaults: map[string]json.RawMessage{"schema_version": json.RawMessage("1")},
	}, own, install)
}
