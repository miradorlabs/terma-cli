package cursor

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func cursorToolRun(t *testing.T, env hookrun.Env, hook, turn string, fields map[string]any) {
	t.Helper()
	env.Stdin = strings.NewReader(cursorPayload(t, env, turn, fields))
	handler := postToolUse
	if hook == "postToolUseFailure" {
		handler = postToolUseFailure
	}
	if err := handler(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// Every tool call is one event keyed on Cursor's call id, with name, duration and outcome,
// and none of tool_input, tool_output or error_message.
func TestCursorToolCallsAreRecordedWithoutContent(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
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

	all := hookruntest.Spooled(t, env.Spool)
	if touched := hookruntest.Named(all, semconv.TermaFilesTouchedEvent); len(touched) != 0 {
		t.Fatalf("a tool call attributed files; that is afterFileEdit's job: %+v", touched)
	}
	if obs := hookruntest.Named(all, semconv.TermaSessionObservationEvent); len(obs) != 0 {
		t.Fatalf("a tool call entered the observation stream: %+v", obs)
	}
	evs := hookruntest.Named(all, semconv.TermaToolCallEvent)
	if len(evs) != 4 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	for _, e := range evs {
		a := e.Attrs
		if e.SessionID != "cursor-conversation" || a[hookrun.AttrProjectID] != "project-a" || a[semconv.GenAIMainAgentNameKey] != "cursor" ||
			a[semconv.TermaEvidenceSourceKey] != "cursor_hook" ||
			a[semconv.GenAIRequestModelKey] != "auto" || a[semconv.GenAIResponseModelKey] != "selected-model" || a[semconv.TermaModelParamThinkingKey] != "high" ||
			a[semconv.TermaMainAgentVersionKey] != "2026.09.10-fd3934a" {
			t.Fatalf("bad common attributes: %+v", e)
		}
		if _, ok := a["model_param.secret"]; ok {
			t.Fatal("unlisted model parameter forwarded")
		}
		if _, ok := a[semconv.UserEmailKey]; ok {
			t.Fatal("a tool call is not a principal record")
		}
	}
	first := evs[0].Attrs
	if first[semconv.TermaHookEventKey] != "postToolUse" || first[semconv.GenAIToolNameKey] != "Shell" || first[semconv.GenAIToolCallIDKey] != "call-1" ||
		first[semconv.TermaOperationDurationMsKey] != float64(5432) || first[semconv.TermaOperationStatusKey] != "completed" || first[semconv.TermaTurnIDKey] != "turn-a" {
		t.Fatal(first)
	}
	if _, ok := first[semconv.ErrorTypeKey]; ok {
		t.Fatal("a completed call has no failure type")
	}
	if evs[1].Attrs[semconv.TermaOperationDurationMsKey] != float64(0) || evs[1].Attrs[semconv.GenAIToolNameKey] != "MCP:linear_search" {
		t.Fatal("explicit zero duration or MCP tool name lost:", evs[1].Attrs)
	}
	third := evs[2].Attrs
	if third[semconv.TermaHookEventKey] != "postToolUseFailure" || third[semconv.TermaOperationStatusKey] != "error" || third[semconv.ErrorTypeKey] != "timeout" ||
		third[semconv.TermaOperationInterruptedKey] != false || third[semconv.TermaOperationDurationMsKey] != float64(12) {
		t.Fatal(third)
	}
	fourth := evs[3].Attrs
	if fourth[semconv.ErrorTypeKey] != semconv.ErrorTypeOther || fourth[semconv.TermaOperationInterruptedKey] != true || fourth[semconv.TermaTurnIDKey] != "turn-b" {
		t.Fatal(fourth)
	}
	if _, ok := fourth[semconv.TermaOperationDurationMsKey]; ok {
		t.Fatal("missing duration became a value")
	}
	if _, ok := fourth[semconv.TermaOperationDurationStatusKey]; ok {
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
	t.Parallel()
	env := testEnv(t)
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_name": "Shell", "tool_use_id": "bad id\n", "duration": -5})
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_name": strings.Repeat("x", 300), "tool_use_id": "call-ok", "duration": "12"})
	cursorToolRun(t, env, "postToolUse", "turn", map[string]any{"tool_input": "no name, no id", "duration": 3})
	cursorToolRun(t, env, "postToolUseFailure", "turn", map[string]any{"tool_name": "Grep", "duration": 1.5, "is_interrupt": "yes"})
	evs := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaToolCallEvent)
	if len(evs) != 3 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	a := evs[0].Attrs
	if _, ok := a[semconv.GenAIToolCallIDKey]; ok {
		t.Fatal("unsafe call id accepted")
	}
	if _, ok := a[semconv.TermaOperationDurationMsKey]; ok || a[semconv.TermaOperationDurationStatusKey] != "invalid" || a[semconv.GenAIToolNameKey] != "Shell" {
		t.Fatal(a)
	}
	b := evs[1].Attrs
	if _, ok := b[semconv.GenAIToolNameKey]; ok {
		t.Fatal("oversized tool name accepted")
	}
	if b[semconv.GenAIToolCallIDKey] != "call-ok" || b[semconv.TermaOperationDurationStatusKey] != "invalid" {
		t.Fatal(b)
	}
	c := evs[2].Attrs
	if c[semconv.GenAIToolNameKey] != "Grep" || c[semconv.TermaOperationDurationStatusKey] != "invalid" || c[semconv.TermaOperationStatusKey] != "error" || c[semconv.ErrorTypeKey] != semconv.ErrorTypeOther {
		t.Fatal(c)
	}
	if _, ok := c[semconv.GenAIToolCallIDKey]; ok {
		t.Fatal("call id invented")
	}
	if _, ok := c[semconv.TermaOperationInterruptedKey]; ok {
		t.Fatal("non-boolean interrupt flag accepted")
	}
}
