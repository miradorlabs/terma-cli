package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The hooks section is the hooks probe's where the build had one, and the plain turn's hooks
// (hooks-first-turn) as {events: …} where it had not.
func TestHooksSection(t *testing.T) {
	first := json.RawMessage(`{"events":{"Stop":{"keys":["hook_event_name"],"count":1}}}`)
	probe := json.RawMessage(`{"supported":["PreToolUse","Stop"],"source":"binary","events":{"Stop":{"fired":true,"count":2,"keys":["hook_event_name"],"kinds":{"hook_event_name":"text"},"env":["CLAUDE_PROJECT_DIR"]}}}`)
	plain := claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "a-2025", "")
	plain.Probes = map[string]json.RawMessage{"hooks-first-turn": first}
	probed := e2e.Profile{Harness: "claude", Version: "1.0.0", Scenario: "TestClaudeProbeHooks", Platform: plain.Platform, At: plain.At,
		Requests: []e2e.ProviderRequest{}, Probes: map[string]json.RawMessage{"hooks": probe}}

	s := night{now: time.Now(), profiles: []e2e.Profile{plain, probed}}.tonight(buildID{"claude", "1.0.0"})
	if sup, _ := s.Hooks["supported"].([]any); len(sup) != 2 || s.Hooks["source"] != "binary" {
		t.Errorf("with a hooks probe, hooks = %v", s.Hooks)
	}
	s = night{now: time.Now(), profiles: []e2e.Profile{plain}}.tonight(buildID{"claude", "1.0.0"})
	events, _ := s.Hooks["events"].(map[string]any)
	if s.Hooks["supported"] != nil || events["Stop"] == nil {
		t.Errorf("without one, hooks = %v", s.Hooks)
	}
}

// A hooks change links by the event, a payload field by its last part, a variable by its name.
func TestHooksNeedle(t *testing.T) {
	for _, c := range []struct {
		path []string
		want string
	}{
		{[]string{"hooks", "supported", "PreCompact"}, "PreCompact"},
		{[]string{"hooks", "events", "PreToolUse", "keys", "tool_input.command"}, "command"},
		{[]string{"hooks", "events", "PostToolBatch", "keys", "tool_calls[]"}, "tool_calls"},
		{[]string{"hooks", "events", "SessionStart", "env", "CLAUDE_ENV_FILE"}, "CLAUDE_ENV_FILE"},
		{[]string{"hooks", "events", "Stop", "control", "exit_2"}, "Stop"},
		{[]string{"hooks", "source"}, ""},
	} {
		if got := needleOf(&Change{Category: "hooks", Path: c.path}); got != c.want {
			t.Errorf("needleOf(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}
