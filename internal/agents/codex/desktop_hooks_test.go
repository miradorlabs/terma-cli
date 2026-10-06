package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A Codex session's hooks send only what Codex's own telemetry lacks: its model calls,
// turns, prompts and tool calls reach the relay natively, so terma sending them too
// counted each twice (#95). Hosted actions, compactions and approval requests stay, with
// content as the team's policy decides.
func TestCodexHooksSendOnlyWhatCodexDoesNotReport(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "redacted", true: "content"}[allow], func(t *testing.T) {
			env := fundingEnv(t)
			env.Policy.IncludePrompts, env.Policy.IncludeToolContent = allow, allow
			routeCodex(t, &env)
			rollout := sequenceFixture(t, strings.Join([]string{
				`{"type":"session_meta","payload":{"id":"` + testCodexID + `"}}`,
				`{"timestamp":"2026-09-19T18:18:39Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
				strings.TrimSuffix(turnContext(replyTurn, env.Cwd), "\n"),
				`{"timestamp":"2026-09-19T18:18:41Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurn + `","item":{"type":"Extension","id":"item_web","kind":"web.search","query":"private query"}}}`,
				strings.TrimSuffix(replyMessage("assistant", "msg_1", "final_answer", "2026-09-19T18:18:42.000Z", "done"), "\n"),
				`{"timestamp":"2026-09-19T18:18:42Z","type":"token_usage_record","payload":{"turn_id":"` + replyTurn + `","response_id":"resp_1","usage":{"input_tokens":100,"output_tokens":40}}}`,
				`{"timestamp":"2026-09-19T18:18:43Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurn + `","started_at_ms":1790000001000,"completed_at_ms":1790000001220,"item":{"type":"ContextCompaction","id":"item_compact"}}}`,
				`{"timestamp":"2026-09-19T18:18:44Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `","duration_ms":4400}}`,
			}, "\n")+"\n")
			run := func(input map[string]any, fn func(context.Context, hookrun.Env) error) {
				input["session_id"], input["cwd"], input["turn_id"], input["transcript_path"] = testCodexID, env.Cwd, replyTurn, rollout
				b, _ := json.Marshal(input)
				env.Stdin = strings.NewReader(string(b))
				if err := fn(context.Background(), env); err != nil {
					t.Fatal(err)
				}
			}
			run(map[string]any{"source": "exec"}, sessionStart)
			run(map[string]any{"prompt": "private prompt"}, userPromptSubmit)
			run(map[string]any{"tool_name": "functions.exec", "permission_mode": "default",
				"tool_input": map[string]any{"description": "private approval reason"}}, permissionRequest)
			run(map[string]any{"tool_name": "functions.exec", "tool_use_id": "call_1",
				"tool_input":    map[string]any{"command": "private command"},
				"tool_response": map[string]any{"exit_code": 0, "output": "private output"}}, postToolUse)
			run(map[string]any{}, stop)
			all := delivered(env, hookruntest.Spooled(t, env.Spool))
			for _, name := range []string{"terma.model.call", "terma.turn.summary", "terma.user.prompt"} {
				if got := hookruntest.Named(all, name); len(got) != 0 {
					t.Errorf("%s sent, which Codex reports itself: %+v", name, got)
				}
			}
			replies := hookruntest.Named(all, semconv.TermaAssistantMessageEvent)
			if allow && (len(replies) != 1 || replies[0].Attrs[semconv.TermaCaptureSurfaceKey] != nil || replies[0].TraceID != replyTrace) {
				t.Fatalf("replies %+v, want one untagged, under the turn's trace id", replies)
			}
			if !allow && len(replies) != 0 {
				t.Fatalf("replies %+v sent while the policy withholds prompts", replies)
			}
			calls, compactions, approvals := hookruntest.Named(all, semconv.TermaToolCallEvent), hookruntest.Named(all, semconv.TermaCompactionEvent), hookruntest.Named(all, semconv.TermaApprovalRequestedEvent)
			if len(calls) != 1 || calls[0].Attrs[semconv.GenAIToolNameKey] != "Extension:web.search" {
				t.Fatalf("tool calls %+v, want only the hosted action", calls)
			}
			if len(compactions) != 1 || len(approvals) != 1 {
				t.Fatalf("compactions %+v, approvals %+v", compactions, approvals)
			}
			for _, e := range append(append(calls, compactions...), approvals...) {
				if e.Attrs[semconv.TermaCaptureSurfaceKey] != semconv.TermaCaptureSurfaceDesktop || e.Attrs[hookrun.AttrProjectID] != "project-a" {
					t.Fatalf("%s untagged or without its project: %+v", e.Name, e.Attrs)
				}
			}
			if approvals[0].Attrs[semconv.TermaApprovalPermissionModeKey] != "default" || approvals[0].Attrs[semconv.TermaObservationIDKey] == "" {
				t.Fatalf("approval request metadata: %+v", approvals[0])
			}
			_, hasReason := approvals[0].Attrs[semconv.TermaApprovalReasonKey]
			_, hasQuery := calls[0].Attrs[semconv.GenAIToolCallArgumentsKey]
			if hasReason != allow || hasQuery != allow {
				t.Fatalf("content under the team's policy: %+v %+v", approvals[0], calls[0])
			}
		})
	}
}

// Codex's PermissionRequest input names only the turn, so the approval takes the trace id
// from the rollout's current turn, under which the backend files the rest of the turn (#100).
func TestCodexApprovalCarriesTheTurnsTraceID(t *testing.T) {
	for _, tc := range []struct {
		name, rollout, turn string
		want                string
	}{
		{"current turn", replyTaskStarted(replyTurnA, replyTraceA) + replyTaskComplete(replyTurnA) + replyTaskStarted(replyTurnB, replyTraceB), replyTurnB, replyTraceB},
		{"another turn", replyTaskStarted(replyTurnA, replyTraceA), replyTurnB, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := fundingEnv(t)
			routeCodex(t, &env)
			rollout := sequenceFixture(t, `{"type":"session_meta","payload":{"id":"`+testCodexID+`"}}`+"\n"+tc.rollout)
			for _, fn := range []func(context.Context, hookrun.Env) error{sessionStart, permissionRequest} {
				b, _ := json.Marshal(map[string]any{"session_id": testCodexID, "cwd": env.Cwd, "turn_id": tc.turn, "transcript_path": rollout, "tool_name": "functions.exec"})
				env.Stdin = strings.NewReader(string(b))
				if err := fn(context.Background(), env); err != nil {
					t.Fatal(err)
				}
			}
			approvals := hookruntest.Named(delivered(env, hookruntest.Spooled(t, env.Spool)), semconv.TermaApprovalRequestedEvent)
			if len(approvals) != 1 || approvals[0].TraceID != tc.want || approvals[0].Attrs[semconv.TermaTurnIDKey] != tc.turn {
				t.Fatalf("approvals %+v, want trace id %v", approvals, tc.want)
			}
		})
	}
}
