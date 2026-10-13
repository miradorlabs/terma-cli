package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const sentinelLoopPrompt = "NEVER-EXPORT-LOOP-PROMPT"

// loopHooks runs Claude Code's hooks for one session against one state directory and spool,
// as successive hook processes would.
type loopHooks struct {
	t        *testing.T
	root     string
	stateDir string
	sp       *spool.Spool
}

func newLoopHooks(t *testing.T) *loopHooks {
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &loopHooks{t: t, root: newRepo(t), stateDir: t.TempDir(), sp: sp}
}

func (h *loopHooks) run(handler func(context.Context, hookrun.Env) error, payload map[string]any) {
	h.t.Helper()
	payload["session_id"], payload["cwd"] = "sess-loop-1", h.root
	b, _ := json.Marshal(payload)
	var stdout bytes.Buffer
	env := hookrun.Env{StateDir: h.stateDir, Now: time.Now(), Cwd: h.root, Policy: hookruntest.Admitting(h.root),
		Stdin: bytes.NewReader(b), Stdout: &stdout, Spool: h.sp, Version: "test"}
	if err := handler(context.Background(), env); err != nil {
		h.t.Fatalf("a hook must never fail: %v", err)
	}
	if stdout.Len() > 0 {
		h.t.Fatalf("UserPromptSubmit's stdout reaches the model, yet the hook printed %q", stdout.String())
	}
}

func (h *loopHooks) submit(promptID, source, prompt string) {
	h.t.Helper()
	payload := map[string]any{"hook_event_name": "UserPromptSubmit", "prompt_id": promptID, "prompt": prompt}
	if source != "" {
		payload["source"] = source
	}
	h.run(userPromptSubmit, payload)
}

func (h *loopHooks) stop(crons any) {
	h.t.Helper()
	payload := map[string]any{"hook_event_name": "Stop", "stop_hook_active": false}
	if crons != nil {
		payload["session_crons"] = crons
	}
	h.run(stop, payload)
}

// loopIDs spools what was submitted so far and returns each iteration's loop id by its turn.
func (h *loopHooks) loopIDs() map[string]string {
	h.t.Helper()
	ids := map[string]string{}
	for _, e := range hookruntest.Named(hookruntest.Spooled(h.t, h.sp), semconv.TermaScheduledLoopIterationEvent) {
		turn, _ := e.Attrs[semconv.TermaTurnIDKey].(string)
		ids[turn], _ = e.Attrs[semconv.TermaScheduledLoopIDKey].(string)
	}
	return ids
}

// A woken turn is reported with its turn, the agent's word for what woke it and a random loop
// id; nothing of the prompt leaves.
func TestClaudeScheduledLoopIterationCarriesTheTurnAndTrigger(t *testing.T) {
	h := newLoopHooks(t)
	h.submit("prompt-1", "loop_wakeup", sentinelLoopPrompt)

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, h.sp))
	if len(events) != 1 || events[0].Name != semconv.TermaScheduledLoopIterationEvent {
		t.Fatalf("events = %s, want one %s", hookruntest.Names(events), semconv.TermaScheduledLoopIterationEvent)
	}
	ev := events[0]
	if ev.SessionID != "sess-loop-1" {
		t.Fatalf("session = %q", ev.SessionID)
	}
	want := map[string]any{
		semconv.GenAIMainAgentNameKey: "claude-code", semconv.TermaTurnIDKey: "prompt-1",
		semconv.TermaScheduledLoopTriggerKey: "loop_wakeup", semconv.TermaEvidenceSourceKey: "claude_hook",
	}
	for k, v := range want {
		if ev.Attrs[k] != v {
			t.Errorf("%s = %v, want %v", k, ev.Attrs[k], v)
		}
	}
	if id, _ := ev.Attrs[semconv.TermaScheduledLoopIDKey].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("loop id = %q, want 32 lowercase hex characters", id)
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), sentinelLoopPrompt) || strings.Contains(string(b), hookrun.EvidenceID("loop_wakeup")) {
		t.Fatalf("the spooled record carries the prompt or the local key: %s", b)
	}
}

// Only a turn a schedule woke is reported, and only one that names its turn.
func TestClaudeScheduledLoopIgnoresOtherTurns(t *testing.T) {
	h := newLoopHooks(t)
	for _, source := range []string{"", "user", "sdk", "system", "poll_event", "something_new"} {
		h.submit("prompt-"+source, source, "hello")
	}
	h.submit("", "loop_wakeup", "no turn id")
	if events := hookruntest.Lifecycle(hookruntest.Spooled(t, h.sp)); len(events) != 0 {
		t.Fatalf("spooled %s for turns no schedule woke", hookruntest.Names(events))
	}
}

// Every wake-up of the self-paced loop shares one id whatever prompt it passes itself; each
// scheduled task has its own, kept across its fires and apart from the self-paced loop's.
func TestClaudeScheduledLoopIDGroupsALoopsIterations(t *testing.T) {
	h := newLoopHooks(t)
	h.submit("p1", "loop_wakeup", "<<autonomous-loop-dynamic>> preamble and tick")
	h.submit("p2", "schedule_wakeup", "check the deploy")
	h.submit("p3", "loop_wakeup", "tick")
	h.submit("p4", "schedule_wakeup", "check the deploy")
	h.submit("p5", "schedule_wakeup", "summarize the inbox")
	h.stop([]any{map[string]any{"id": "c1", "schedule": "*/5 * * * *", "recurring": true, "prompt": "check the deploy"}})
	h.stop(nil)
	h.submit("p6", "loop_wakeup", "tick")

	ids := h.loopIDs()
	if len(ids) != 6 {
		t.Fatalf("iterations = %v, want six", ids)
	}
	selfPaced, deploy, inbox := ids["p1"], ids["p2"], ids["p5"]
	if ids["p3"] != selfPaced || ids["p6"] != selfPaced {
		t.Errorf("self-paced wake-ups split across ids: %v", ids)
	}
	if ids["p4"] != deploy {
		t.Errorf("a scheduled task's fires split across ids: %v", ids)
	}
	if selfPaced == deploy || deploy == inbox || selfPaced == inbox {
		t.Errorf("distinct loops share an id: %v", ids)
	}
}

// Once Stop says nothing is left to wake the session, its loops are over: the next one is new.
func TestClaudeScheduledLoopEndsWhenNothingIsScheduled(t *testing.T) {
	h := newLoopHooks(t)
	h.submit("p1", "loop_wakeup", "tick")
	h.stop([]any{})
	h.submit("p2", "loop_wakeup", "tick")

	ids := h.loopIDs()
	if ids["p1"] == "" || ids["p1"] == ids["p2"] {
		t.Fatalf("a loop begun after the last one ended reused its id: %v", ids)
	}
}
