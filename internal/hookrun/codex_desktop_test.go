package hookrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/shim"
)

func TestCodexDesktopHooksCaptureLocalToolsWithRepositoryConsent(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "redacted", true: "content"}[allow], func(t *testing.T) {
			env := fundingEnv(t)
			desktop := true
			if err := shim.SaveRecord(shim.Record{ProjectID: "project-a", Endpoint: "https://otel.terma.ai",
				Signals: []string{"logs"}, Harnesses: []string{shim.AgentCodex}, Desktop: &desktop,
				IncludePrompts: allow, IncludeToolContent: allow}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(shim.CodexRoutedEnv, "")
			run := func(input map[string]any, fn func(context.Context, Env) error) {
				b, _ := json.Marshal(input)
				env.Stdin = strings.NewReader(string(b))
				if err := fn(context.Background(), env); err != nil {
					t.Fatal(err)
				}
			}
			run(map[string]any{"session_id": replySession, "cwd": env.Cwd, "turn_id": replyTurn, "prompt": "private prompt"}, CodexUserPromptSubmit)
			run(map[string]any{"session_id": replySession, "cwd": env.Cwd, "turn_id": replyTurn,
				"tool_name": "functions.exec", "tool_use_id": "call_1"}, CodexPreToolUse)
			env.Now = env.Now.Add(1500 * time.Millisecond)
			run(map[string]any{"session_id": replySession, "cwd": env.Cwd, "turn_id": replyTurn,
				"tool_name": "functions.exec", "permission_mode": "default",
				"tool_input": map[string]any{"description": "private approval reason"}}, CodexPermissionRequest)
			run(map[string]any{"session_id": replySession, "cwd": env.Cwd, "turn_id": replyTurn,
				"tool_name": "functions.exec", "tool_use_id": "call_1",
				"tool_input":    map[string]any{"command": "private command"},
				"tool_response": map[string]any{"exit_code": 0, "output": "private output"}}, CodexPostToolUse)
			all := spooled(t, env.Spool)
			prompts, calls, approvals := eventsNamed(all, EventUserPrompt), eventsNamed(all, EventToolCall), eventsNamed(all, EventApprovalAsked)
			if len(prompts) != 1 || len(calls) != 1 || len(approvals) != 1 {
				t.Fatalf("prompt/call missing: %+v", all)
			}
			if calls[0].Attrs["duration_ms"] != float64(1500) || calls[0].Attrs["duration_source"] != "hook_elapsed" {
				t.Fatalf("observed tool duration: %+v", calls[0])
			}
			if approvals[0].Attrs["permission_mode"] != "default" || approvals[0].Attrs["observation_id"] == "" {
				t.Fatalf("approval request metadata: %+v", approvals[0])
			}
			_, hasReason := approvals[0].Attrs["reason"]
			if hasReason != allow {
				t.Fatalf("approval reason consent: %+v", approvals[0])
			}
			if prompts[0].Attrs["capture_surface"] != "desktop" || calls[0].Attrs["tool_call_id"] != "call_1" {
				t.Fatalf("route/identity: %+v %+v", prompts[0], calls[0])
			}
			_, hasPrompt := prompts[0].Attrs["prompt"]
			_, hasArgs := calls[0].Attrs["arguments"]
			_, hasOutput := calls[0].Attrs["output"]
			if hasPrompt != allow || hasArgs != allow || hasOutput != allow {
				t.Fatalf("consent failed: %+v %+v", prompts[0], calls[0])
			}
		})
	}
}
