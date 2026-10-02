package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func TestPolicyBlocksCodexReplyCapture(t *testing.T) {
	for _, mode := range []string{config.ModeRepo, config.ModeGlobal} {
		for _, rule := range []string{"prompts", "paths", "agent"} {
			t.Run(mode+"/"+rule, func(t *testing.T) {
				env := fundingEnv(t)
				env.Policy.Mode = mode
				routeCodex(t)
				switch rule {
				case "prompts":
					env.Policy.IncludePrompts = false
				case "paths":
					env.Policy.ExcludePaths = []string{".env"}
				case "agent":
					rec, _, err := routing.LoadRecord("project-a")
					if err != nil {
						t.Fatal(err)
					}
					rec.Harnesses = []string{"claude"}
					if err := routing.SaveRecord(rec); err != nil {
						t.Fatal(err)
					}
				}
				if replies := stopCodex(t, env, replyRollout(t)); len(replies) != 0 {
					t.Fatalf("%d private replies spooled", len(replies))
				}
			})
		}
	}
}

// The team's policy alone decides what Codex's hook events carry: they reach the spool,
// not the relay, so nothing else would withhold it.
func TestTeamPolicyWithholdsCodexHookContent(t *testing.T) {
	for label, team := range map[string]config.Policy{
		"prompts and tool content off": {Mode: config.ModeRepo},
		"paths excluded":               {Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, ExcludePaths: []string{"secrets/**"}},
	} {
		t.Run(label, func(t *testing.T) {
			env := fundingEnv(t)
			team.TeamID, team.FetchedAt = "project-a", time.Now()
			if err := routing.SavePolicy(team); err != nil {
				t.Fatal(err)
			}
			if err := routing.SaveRecord(routing.Record{ProjectID: "project-a", Endpoint: "https://otel.terma.ai",
				Signals: []string{"logs"}, Harnesses: []string{name}, Surfaces: []string{name}}); err != nil {
				t.Fatal(err)
			}
			rollout := sequenceFixture(t, `{"timestamp":"2026-09-19T18:18:41Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"`+replyTurn+`","item":{"type":"Extension","id":"item_web","kind":"web.search","query":"private query"}}}`+"\n")
			run := func(input map[string]any, fn func(context.Context, hookrun.Env) error) {
				input["session_id"], input["cwd"], input["turn_id"], input["transcript_path"] = testCodexID, env.Cwd, replyTurn, rollout
				b, _ := json.Marshal(input)
				env.Stdin = strings.NewReader(string(b))
				if err := fn(context.Background(), env); err != nil {
					t.Fatal(err)
				}
			}
			run(map[string]any{"prompt": "private prompt"}, userPromptSubmit)
			run(map[string]any{"tool_name": "functions.exec", "tool_input": map[string]any{"description": "private reason"}}, permissionRequest)
			run(map[string]any{"tool_name": "functions.exec", "tool_use_id": "call_1",
				"tool_input":    map[string]any{"command": "private command"},
				"tool_response": map[string]any{"exit_code": 0, "output": "private output"}}, postToolUse)
			run(map[string]any{}, stop)
			all := hookruntest.Spooled(t, env.Spool)
			prompts, calls := hookruntest.Named(all, hookrun.EventUserPrompt), hookruntest.Named(all, hookrun.EventToolCall)
			if len(prompts) != 1 || len(calls) != 2 || len(hookruntest.Named(all, hookrun.EventApprovalAsked)) != 1 {
				t.Fatalf("events missing: %+v", all)
			}
			for _, ev := range all {
				for _, key := range []string{"prompt", "arguments", "output", "reason"} {
					if _, ok := ev.Attrs[key]; ok && (ev.Name != hookrun.EventSessionEnd || key != "reason") {
						t.Fatalf("%s carries %s against the team's policy: %+v", ev.Name, key, ev.Attrs)
					}
				}
				if b, _ := json.Marshal(ev.Attrs); strings.Contains(string(b), "private") {
					t.Fatalf("%s carries private content: %s", ev.Name, b)
				}
			}
		})
	}
}
