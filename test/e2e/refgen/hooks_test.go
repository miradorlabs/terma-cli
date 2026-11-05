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

// What an event was handed is published with its values, normalized as the rest of the
// snapshot is: the sandbox's paths, the session, ids and times as placeholders.
func TestHooksExample(t *testing.T) {
	probe := json.RawMessage(`{"supported":["Stop"],"source":"binary","events":{"Stop":{"fired":true,"count":1,"keys":["cwd","hook_event_name","session_id","transcript_path"],` +
		`"kinds":{"cwd":"text","hook_event_name":"text","session_id":"text","transcript_path":"text"},"env":["CLAUDE_PROJECT_DIR","CLAUDE_CODE_MESSAGING_TOKEN"],` +
		`"example":{"cwd":"/private/var/folders/xy/T/Test1/001/repo","hook_event_name":"Stop","session_id":"9b1f6f0e-1111-4222-8333-444455556666",` +
		`"transcript_path":"/var/folders/xy/T/Test1/001/claude/projects/-var-folders-xy-T-Test1-001-repo/9b1f6f0e-1111-4222-8333-444455556666.jsonl",` +
		`"agent_id":"a7c2d9e1-0000-4000-8000-000000000001","at":"2026-11-04T04:41:09Z"},` +
		`"env_values":{"CLAUDE_PROJECT_DIR":"/var/folders/xy/T/Test1/001/repo","CLAUDE_CODE_MESSAGING_TOKEN":"<redacted>"}}}}`)
	base := claudeProfile("1.0.0", "Be brief.", []string{"Bash"}, "a-2025", "")
	probed := e2e.Profile{Harness: "claude", Version: "1.0.0", Scenario: "TestClaudeProbeHooks", Platform: base.Platform, At: base.At,
		Requests: []e2e.ProviderRequest{}, Probes: map[string]json.RawMessage{"hooks": probe},
		Placeholders: map[string]string{"/var/folders/xy/T/Test1/001/repo": "<repo>", "/var/folders/xy/T/Test1/001": "<sandbox>",
			"/var/folders/xy/T/Test1/001/claude": "<claude-config>", "9b1f6f0e-1111-4222-8333-444455556666": "<session>"}}
	s := night{now: time.Now(), profiles: []e2e.Profile{base, probed}}.tonight(buildID{"claude", "1.0.0"})
	events, _ := s.Hooks["events"].(map[string]any)
	stop, _ := events["Stop"].(map[string]any)
	ex, _ := stop["example"].(map[string]any)
	for k, want := range map[string]string{"cwd": "<repo>", "session_id": "<session>", "agent_id": "<uuid>",
		"transcript_path": "<claude-config>/projects/<repo-slug>/<session>.jsonl"} {
		if ex[k] != want {
			t.Errorf("example %s = %v, want %s", k, ex[k], want)
		}
	}
	values, _ := stop["env_values"].(map[string]any)
	if values["CLAUDE_PROJECT_DIR"] != "<repo>" || values["CLAUDE_CODE_MESSAGING_TOKEN"] != "<redacted>" {
		t.Errorf("env_values = %v", values)
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

// A process id a hook's variable held is the run's: <pid>.
func TestHooksPIDsPlaced(t *testing.T) {
	h := pidsPlaced(map[string]any{"events": map[string]any{"PreToolUse": map[string]any{
		"env_values": map[string]any{"CLAUDE_PID": "82807", "CLAUDE_EFFORT": "medium", "PID_FILE": "/x"}}}})
	vals := h["events"].(map[string]any)["PreToolUse"].(map[string]any)["env_values"].(map[string]any)
	if vals["CLAUDE_PID"] != "<pid>" || vals["CLAUDE_EFFORT"] != "medium" || vals["PID_FILE"] != "/x" {
		t.Errorf("env_values = %v", vals)
	}
}
