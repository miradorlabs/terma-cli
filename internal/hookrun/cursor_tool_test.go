package hookrun

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

func cursorToolRun(t *testing.T, env Env, hook, turn string, fields map[string]any) {
	t.Helper()
	env.Stdin = strings.NewReader(cursorPayload(t, env, turn, fields))
	handler := CursorPostToolUse
	if hook == "postToolUseFailure" {
		handler = CursorPostToolUseFailure
	}
	if err := handler(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

func eventsNamed(events []spool.Event, name string) []spool.Event {
	var out []spool.Event
	for _, e := range events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// Every tool call Cursor reports is one event keyed on Cursor's own call id, carrying
// the tool's name, duration and outcome — and none of what the tool was given or gave
// back. Hook payloads carry tool_input, tool_output and error_message; the event never does.
func TestCursorToolCallsAreRecordedWithoutContent(t *testing.T) {
	env := fundingEnv(t)
	private := map[string]any{
		"tool_input": map[string]any{"command": "secret-command"}, "tool_output": "secret-output",
		"error_message": "secret-error", "agent_message": "secret-note", "cwd": "/secret/cwd",
		"model_params": []map[string]string{{"id": "thinking", "value": "high"}, {"id": "secret", "value": "x"}},
	}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		maps.Copy(m, private)
		maps.Copy(m, extra)
		return m
	}
	cursorToolRun(t, env, "postToolUse", "turn-a", with(map[string]any{"tool_name": "Shell", "tool_use_id": "call-1", "duration": 5432}))
	cursorToolRun(t, env, "postToolUse", "turn-a", with(map[string]any{"tool_name": "MCP:linear_search", "tool_use_id": "call-2", "duration": 0}))
	cursorToolRun(t, env, "postToolUseFailure", "turn-a", with(map[string]any{"tool_name": "Read", "tool_use_id": "call-3", "duration": 12, "failure_type": "timeout", "is_interrupt": false}))
	cursorToolRun(t, env, "postToolUseFailure", "turn-b", with(map[string]any{"tool_name": "Write", "tool_use_id": "call-4", "failure_type": "secret-kind", "is_interrupt": true}))

	all := spooledQuota(t, env.Spool)
	if touched := eventsNamed(all, EventFilesTouched); len(touched) != 0 {
		t.Fatalf("a tool call attributed files; that is afterFileEdit's job: %+v", touched)
	}
	if obs := eventsNamed(all, EventSessionObservation); len(obs) != 0 {
		t.Fatalf("a tool call entered the observation stream: %+v", obs)
	}
	evs := eventsNamed(all, EventToolCall)
	if len(evs) != 4 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	for _, e := range evs {
		a := e.Attrs
		if e.SessionID != "cursor-conversation" || a[AttrProjectID] != "project-a" || a["tool"] != "cursor" ||
			a["evidence_source"] != "cursor_hook" || a["ordering"] != "local_receipt" || a["schema_version"] != float64(1) ||
			a["model"] != "auto" || a["model_id"] != "selected-model" || a["model_param.thinking"] != "high" ||
			a["cursor.version"] != "2026.09.10-fd3934a" || a["terma.version"] != "test" {
			t.Fatalf("bad common attributes: %+v", e)
		}
		if _, ok := a["model_param.secret"]; ok {
			t.Fatal("unlisted model parameter forwarded")
		}
		if _, ok := a["account_email"]; ok {
			t.Fatal("a tool call is not a principal record")
		}
	}
	first := evs[0].Attrs
	if first["hook_event"] != "postToolUse" || first["tool_name"] != "Shell" || first["tool_call_id"] != "call-1" ||
		first["duration_ms"] != float64(5432) || first["status"] != "completed" || first["turn_id"] != "turn-a" {
		t.Fatal(first)
	}
	if _, ok := first["failure_type"]; ok {
		t.Fatal("a completed call has no failure type")
	}
	if evs[1].Attrs["duration_ms"] != float64(0) || evs[1].Attrs["tool_name"] != "MCP:linear_search" {
		t.Fatal("explicit zero duration or MCP tool name lost:", evs[1].Attrs)
	}
	third := evs[2].Attrs
	if third["hook_event"] != "postToolUseFailure" || third["status"] != "error" || third["failure_type"] != "timeout" ||
		third["is_interrupt"] != false || third["duration_ms"] != float64(12) {
		t.Fatal(third)
	}
	fourth := evs[3].Attrs
	if fourth["failure_type"] != "unknown" || fourth["is_interrupt"] != true || fourth["turn_id"] != "turn-b" {
		t.Fatal(fourth)
	}
	if _, ok := fourth["duration_ms"]; ok {
		t.Fatal("missing duration became a value")
	}
	if _, ok := fourth["duration_status"]; ok {
		t.Fatal("missing duration reported as invalid")
	}
	b, _ := json.Marshal(evs)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "/cwd") {
		t.Fatalf("private content escaped: %s", b)
	}
}

// Bad values are dropped one at a time, never repaired, and a payload that names
// neither a tool nor a call is not an event.
func TestCursorToolCallValidation(t *testing.T) {
	env := fundingEnv(t)
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_name": "Shell", "tool_use_id": "bad id\n", "duration": -5})
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_name": strings.Repeat("x", 300), "tool_use_id": "call-ok", "duration": "12"})
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_input": "no name, no id", "duration": 3})
	cursorToolRun(t, env, "postToolUseFailure", "turn", map[string]any{"tool_name": "Grep", "duration": 1.5, "is_interrupt": "yes"})
	evs := eventsNamed(spooledQuota(t, env.Spool), EventToolCall)
	if len(evs) != 3 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	a := evs[0].Attrs
	if _, ok := a["tool_call_id"]; ok {
		t.Fatal("unsafe call id accepted")
	}
	if _, ok := a["duration_ms"]; ok || a["duration_status"] != "invalid" || a["tool_name"] != "Shell" {
		t.Fatal(a)
	}
	b := evs[1].Attrs
	if _, ok := b["tool_name"]; ok {
		t.Fatal("oversized tool name accepted")
	}
	if b["tool_call_id"] != "call-ok" || b["duration_status"] != "invalid" {
		t.Fatal(b)
	}
	c := evs[2].Attrs
	if c["tool_name"] != "Grep" || c["duration_status"] != "invalid" || c["status"] != "error" || c["failure_type"] != "unknown" {
		t.Fatal(c)
	}
	if _, ok := c["tool_call_id"]; ok {
		t.Fatal("call id invented")
	}
	if _, ok := c["is_interrupt"]; ok {
		t.Fatal("non-boolean interrupt flag accepted")
	}
}
