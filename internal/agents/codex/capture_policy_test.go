package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestPolicyBlocksCodexReplyCapture(t *testing.T) {
	for _, mode := range []string{config.ModeRepo, config.ModeGlobal} {
		for _, rule := range []string{"prompts", "agent"} {
			if mode == config.ModeGlobal && rule == "agent" {
				continue // global mode collects every agent's sessions
			}
			t.Run(mode+"/"+rule, func(t *testing.T) {
				env := fundingEnv(t)
				env.Policy.Mode = mode
				routeCodex(t, &env)
				switch rule {
				case "prompts":
					env.Policy.IncludePrompts = false
				case "agent":
					if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{"claude"} }); err != nil {
						t.Fatal(err)
					}
					env.Agents = []string{"claude"}
				}
				if replies := delivered(env.Policy, stopCodex(t, env, replyRollout(t))); len(replies) != 0 {
					t.Fatalf("%d private replies delivered", len(replies))
				}
			})
		}
	}
}

// delivered is what the spool's delivery sends of events under org, the team's policy
// when they leave, asking Codex's own consent as the command line does.
func delivered(org config.Policy, events []spool.Event) []spool.Event {
	r := delivery.Router{Consent: func(_ string, c hookrun.Consent) bool { return repliesConsented(c) }}
	var out []spool.Event
	for _, e := range events {
		if sent, ok := r.Outgoing(org, "project-a", e); ok {
			out = append(out, sent)
		}
	}
	return out
}

// The team's policy alone decides what Codex's hook events carry when they leave. They
// reach the spool, not the relay, so the hooks record them whole and delivery withholds
// what the policy does not collect.
func TestTeamPolicyWithholdsCodexHookContent(t *testing.T) {
	for label, team := range map[string]config.Policy{
		"prompts and tool content off": {Mode: config.ModeRepo},
	} {
		t.Run(label, func(t *testing.T) {
			env := fundingEnv(t)
			team.TeamID, team.FetchedAt = "project-a", time.Now()
			if err := routing.SavePolicy(team); err != nil {
				t.Fatal(err)
			}
			routeCodex(t, &env)
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
			spooled := hookruntest.Spooled(t, env.Spool)
			if b, _ := json.Marshal(spooled); !strings.Contains(string(b), "private prompt") {
				t.Fatalf("the hooks did not record the prompt whole: %s", b)
			}
			sent := delivered(env.Policy, spooled)
			prompts, calls := hookruntest.Named(sent, hookrun.EventUserPrompt), hookruntest.Named(sent, hookrun.EventToolCall)
			if len(prompts) != 1 || len(calls) != 2 || len(hookruntest.Named(sent, hookrun.EventApprovalAsked)) != 1 {
				t.Fatalf("events not delivered: %+v", sent)
			}
			for _, ev := range sent {
				for _, key := range []string{"prompt", "arguments", "output", "reason"} {
					if _, ok := ev.Attrs[key]; ok && (ev.Name != hookrun.EventSessionEnd || key != "reason") {
						t.Fatalf("%s left with %s against the team's policy: %+v", ev.Name, key, ev.Attrs)
					}
				}
				if b, _ := json.Marshal(ev.Attrs); strings.Contains(string(b), "private") {
					t.Fatalf("%s left with private content: %s", ev.Name, b)
				}
			}
		})
	}
}
