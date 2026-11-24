package antigravity

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// What a conversation said leaves with the turn it was said in: the prompt, each reply, each
// tool call's arguments and each tool's result, under the content policy and the developer's
// consent. The rig's fixture holds made-up text in agy's shapes (fill).

// toolUse is a PostToolUse payload's part for a call at step with args, a JSON object.
func toolUse(step int, name, args string) string {
	return `"stepIdx":` + strconv.Itoa(step) + `,"toolCall":{"name":"` + name + `","args":` + args + `}`
}

// turnOne runs the fixture's first turn as agy runs its hooks: a write at step 2 and a
// command at step 4, each result written after its PostToolUse, and the reply at step 6.
func (g *transcriptRig) turnOne() {
	g.t.Helper()
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "write_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(g.root, "a.txt"))+`","Content":"made-up content","Overwrite":true}`))
	g.write(4)
	g.hook(postToolUse, toolUse(4, "run_command", `{"CommandLine":"made-up command","WaitMsBeforeAsync":5000}`))
	g.write(7)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
}

// byID is the events of a kind by their id (message id, or tool call id).
func byID(events []spool.Event) map[string]spool.Event {
	out := map[string]spool.Event{}
	for _, e := range events {
		id, _ := e.Attrs[semconv.TermaMessageIDKey].(string)
		if id == "" {
			id, _ = e.Attrs[semconv.GenAIToolCallIDKey].(string)
		}
		out[id] = e
	}
	return out
}

// sentOnce fails when a prompt, reply or result went twice under one id.
func (g *transcriptRig) sentOnce() {
	g.t.Helper()
	seen := map[string]bool{}
	for _, e := range g.seen {
		switch e.Name {
		case semconv.TermaUserMessageEvent, semconv.TermaAssistantMessageEvent, semconv.TermaToolResultEvent:
			id, _ := e.Attrs[semconv.TermaMessageIDKey].(string)
			if e.Name == semconv.TermaToolResultEvent {
				id, _ = e.Attrs[semconv.GenAIToolCallIDKey].(string)
			}
			key := e.Name + " " + id
			if seen[key] {
				g.t.Errorf("%s went twice", key)
			}
			seen[key] = true
		}
	}
}

func TestAntigravityTurnSendsWhatWasSaid(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	got := g.drain()

	prompts := got[semconv.TermaUserMessageEvent]
	if len(prompts) != 1 {
		t.Fatalf("%d prompts, want 1: %v", len(prompts), prompts)
	}
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityTranscript,
		semconv.TermaMessageIDKey: "step-0", semconv.TermaMessageTextKey: "made-up prompt 0", semconv.TermaMessageTruncatedKey: false,
		semconv.TermaTurnIDKey: "turn-1", semconv.TermaStepIndexKey: int64(0),
	} {
		if v := prompts[0].Attrs[k]; norm(v) != norm(want) {
			t.Errorf("prompt[%s] = %v, want %v", k, v, want)
		}
	}
	replies := byID(got[semconv.TermaAssistantMessageEvent])
	if len(replies) != 1 || replies["step-6"].Attrs[semconv.TermaMessageTextKey] != "made-up reply 6" ||
		replies["step-6"].Attrs[semconv.TermaTurnIDKey] != "turn-1" || norm(replies["step-6"].Attrs[semconv.TermaStepIndexKey]) != norm(int64(6)) ||
		replies["step-6"].Attrs[semconv.GenAIRequestModelKey] != "gemini-3.8-flash-low" || replies["step-6"].Attrs[semconv.TermaMessageTruncatedKey] != false {
		t.Errorf("replies: %v", got[semconv.TermaAssistantMessageEvent])
	}
	calls := byID(got[semconv.TermaToolCallEvent])
	write, _ := calls["step-2"].Attrs[semconv.GenAIToolCallArgumentsKey].(map[string]any)
	command, _ := calls["step-4"].Attrs[semconv.GenAIToolCallArgumentsKey].(map[string]any)
	if write["Content"] != "made-up content" || write["Path"] != filepath.Join(g.root, "a.txt") || command["CommandLine"] != "made-up command" {
		t.Errorf("arguments: %v, %v", write, command)
	}
	results := byID(got[semconv.TermaToolResultEvent])
	for _, step := range []string{"2", "4"} {
		r := results["step-"+step]
		if r.Attrs[semconv.GenAIToolCallResultKey] != "Created At: 2026-11-20T16:51:50Z\nmade-up result "+step || r.Attrs[semconv.TermaTurnIDKey] != "turn-1" ||
			r.Attrs[semconv.TermaEvidenceSourceKey] != sourceAntigravityTranscript || r.SessionID != turnsConversation {
			t.Errorf("result of step %s: %+v", step, r)
		}
		if _, named := r.Attrs[semconv.GenAIToolNameKey]; named {
			t.Errorf("a result names a tool the transcript does not: %v", r.Attrs)
		}
		if !r.Time.After(calls["step-"+step].Time) {
			t.Errorf("the result of step %s sorts before its call", step)
		}
	}
	if len(results) != 2 {
		t.Errorf("results: %v", got[semconv.TermaToolResultEvent])
	}

	// The next turn's prompt goes with it, the envelope off.
	g.write(8)
	g.hook(preInvocation, begin("8"))
	next := g.drain()[semconv.TermaUserMessageEvent]
	if len(next) != 1 || next[0].Attrs[semconv.TermaMessageIDKey] != "step-7" || next[0].Attrs[semconv.TermaTurnIDKey] != "turn-8" ||
		next[0].Attrs[semconv.TermaMessageTextKey] != "made-up prompt 7" {
		t.Errorf("turn 8's prompt: %v", next)
	}
	g.sentOnce()
}

// A tool call's arguments go only with tool content on and Antigravity chosen; the call goes
// either way. What each transcript line sends under the same switches is
// TestAntigravityWhatEachLineSends.
func TestAntigravityArgumentsFollowTheSwitches(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		noPrompts, noToolContent, deselect bool
		args                               bool
	}{
		{name: "everything", args: true},
		{name: "prompts off", noPrompts: true, args: true},
		{name: "tool content off", noToolContent: true},
		{name: "both off", noPrompts: true, noToolContent: true},
		{name: "Antigravity not chosen", deselect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.noPrompts, g.noToolContent = tc.noPrompts, tc.noToolContent
			if tc.deselect {
				g.agents = []string{"codex"}
			}
			g.turnOne()
			got := g.drain()
			if len(got[semconv.TermaToolCallEvent]) != 2 {
				t.Errorf("tool calls %v", got[semconv.TermaToolCallEvent])
			}
			for _, c := range got[semconv.TermaToolCallEvent] {
				if _, args := c.Attrs[semconv.GenAIToolCallArgumentsKey]; args != tc.args {
					t.Errorf("arguments on %v: %v, want %v", c.Attrs[semconv.GenAIToolCallIDKey], args, tc.args)
				}
			}
		})
	}
}

// Content travels when the developer chose Antigravity, or global mode collects every
// session, whatever was chosen.
func TestAntigravityContentConsentIsChosenOrGlobal(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.agents = nil
	env := g.env(g.root, "")
	env.Policy.Mode = "global"
	if !contentConsented(env.Consent(nil)) {
		t.Fatal("global mode is not consent")
	}
	if contentConsented(g.env(g.root, "").Consent(nil)) {
		t.Fatal("no agent chosen is consent")
	}
}

// The prompt is the turn's own: a wake (a new invocation 0 with no new prompt) sends none, a
// prompt joined mid-turn goes with its own step, and a subagent, which has no prompt, sends
// none.
func TestAntigravityPromptsOnlyAsWritten(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	g.drain()
	// A wake: invocation 0 at step 7, after the reply; no USER_INPUT before it.
	g.hook(preInvocation, begin("7"))
	g.hook(stop, stopped)
	if got := g.drain()[semconv.TermaUserMessageEvent]; len(got) != 0 {
		t.Errorf("a wake sent a prompt: %v", got)
	}

	j := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	j.write(8)
	j.hook(preInvocation, begin("8"))
	joined := strings.Replace(j.lines[7], `"step_index":7`, `"step_index":10`, 1)
	j.lines = append(j.lines[:10:10], joined, j.lines[11])
	j.write(len(j.lines))
	j.hook(stop, stopped)
	j.drain()
	var ids []string
	for _, e := range j.seen {
		if e.Name == semconv.TermaUserMessageEvent {
			ids = append(ids, e.Attrs[semconv.TermaMessageIDKey].(string)+" "+e.Attrs[semconv.TermaTurnIDKey].(string))
		}
	}
	if strings.Join(ids, ",") != "step-7 turn-8,step-10 turn-8" {
		t.Errorf("prompts %v", ids)
	}

	c := newTranscriptRig(t, childConversation, "transcript_subagent_child.jsonl")
	c.hook(preInvocation, begin("0"))
	c.write(len(c.lines))
	c.hook(postInvocation, begin("0"))
	c.hook(stop, stopped)
	got := c.drain()
	if len(got[semconv.TermaUserMessageEvent]) != 0 {
		t.Errorf("a subagent sent a prompt: %v", got[semconv.TermaUserMessageEvent])
	}
	if len(got[semconv.TermaAssistantMessageEvent]) != 1 || len(got[semconv.TermaToolResultEvent]) == 0 {
		t.Errorf("the subagent's reply and results: %v, %v", got[semconv.TermaAssistantMessageEvent], got[semconv.TermaToolResultEvent])
	}
}

// A state that begins passes what the transcript holds, but the turn's own prompt, which
// agy wrote just before the turn's first hook.
func TestAntigravityFirstHookSendsItsTurnsPrompt(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	got := g.drain()
	if p := got[semconv.TermaUserMessageEvent]; len(p) != 1 || p[0].Attrs[semconv.TermaMessageTextKey] != "made-up prompt 7" {
		t.Errorf("prompts %v", p)
	}
	for _, step := range []int64{0, 2, 4, 6} {
		if g.spoke(step) {
			t.Errorf("step %d, before terma followed the conversation, was sent", step)
		}
	}
	if !g.spoke(10) || !g.spoke(11) {
		t.Error("turn 8's result and reply did not go")
	}
}

// What was sent before a rewrite (compaction) or a lost state is not sent again.
func TestAntigravityRewriteSendsNoContentTwice(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	g.drain()
	g.lines = append([]string{}, g.lines[7:17]...)
	g.write(6)
	g.hook(preInvocation, begin("13"))
	g.write(len(g.lines))
	g.hook(stop, stopped)
	g.hook(stop, stopped)
	g.drain()
	g.sentOnce()
	if !g.spoke(12) || !g.spoke(16) {
		t.Error("turn 13's prompt and reply did not go after the rewrite")
	}
}
