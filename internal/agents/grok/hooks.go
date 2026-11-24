package grok

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

const (
	// homeEnv moves grok's home directory, ~/.grok, and both files terma writes there.
	homeEnv = "GROK_HOME"
	// hooksFileName is terma's own file in grok's user hooks directory: grok merges every
	// *.json there, so terma writes none of the developer's files.
	hooksFileName = "terma.json"
)

// grokHooks are the hooks terma's file runs. No PreToolUse (a process per tool call for
// nothing), no SubagentStop (it says only that a subagent stopped, not how), and no
// compaction, notification or permission hooks.
var grokHooks = []struct {
	Event   string
	Matcher string
	Hook    string
}{
	{"SessionStart", "", eventSessionStart},
	// The one hook before a turn exports anything, so the relay is up and the session claimed
	// first. It must print nothing: grok hands its stdout to the model.
	{"UserPromptSubmit", "", eventUserPromptSubmit},
	// Only an edit tool's result can prove an edit; the subagent tools' results in the parent
	// say which call launched a subagent and, for a background one, how it ended.
	{"PostToolUse", "search_replace|write|spawn_subagent|get_command_or_subagent_output|kill_command_or_subagent", eventPostToolUse},
	{"SubagentStart", "", eventSubagentStart},
	{"Stop", "", eventStop},
	{"SessionEnd", "", eventSessionEnd},
}

// grokHome is grok's home directory: $GROK_HOME, else ~/.grok.
func grokHome() (string, error) {
	if dir := os.Getenv(homeEnv); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grok"), nil
}

// UserHooksPath is terma's file in grok's user hooks directory, which every grok session on
// the machine loads.
func (Agent) UserHooksPath() (string, error) {
	home, err := grokHome()
	return filepath.Join(home, "hooks", hooksFileName), err
}

// PlanUserHooks writes terma's hooks file into dir, or takes it out; it writes none for a
// grok too old to run them.
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	if why, old := tooOld(context.Background()); install && old {
		return hookmgr.Plan{}, errors.New(why)
	}
	return planUserHooks(dir, command, install)
}

// planUserHooks is terma's file, in Claude Code's shape, which grok reads: each event a list of
// matcher groups. The file is terma's alone; removing the last entry removes it.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	own := make([]hookmgr.EventHook, 0, len(grokHooks))
	for _, h := range grokHooks {
		entry, err := hookmgr.Group(h.Event, h.Matcher, hookCmd{Type: "command", Command: command(h.Hook), Timeout: 10})
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, entry)
	}
	return hookmgr.MergeEventHooks(dir, hookmgr.HooksFile{Path: hooksFileName}, own, install)
}
