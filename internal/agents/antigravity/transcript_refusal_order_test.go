package antigravity

import (
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A hook that runs after its call's result was read, and the order a conversation's events
// leave in.

// A tool whose hook runs after its result was read (agy's schedule) needs no hook for its
// result: it goes when its batch is clean and is withheld when the batch is tainted.
func TestAntigravityResultReadBeforeItsHookFollowsItsBatch(t *testing.T) {
	file := excludedFile(t)
	for _, tc := range []struct {
		name, call string
		sent       bool
	}{
		{"clean", `{"name":"schedule","args":{"Delay":"made-up"}}`, true},
		{"tainted", readCall(file), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.lines[1] = withCalls(g.lines[1], tc.call)
			g.write(1)
			g.hook(preInvocation, begin("1"))
			g.write(4)
			g.hook(postInvocation, invoked)
			if _, result := stepContent(g.drain(), "step-2"); result != tc.sent {
				t.Fatalf("before any hook of the call, result went %v, want %v", result, tc.sent)
			}
			if !tc.sent {
				return
			}
			g.hook(postToolUse, toolUse(2, "schedule", `{"Delay":"made-up"}`))
			if args, _ := stepContent(g.drain(), "step-2"); !args {
				t.Error("the late hook's arguments did not go")
			}
			// The design's known limit (5.6): a call whose hook runs after its result was read is
			// stamped after that result, so in the replay this tool's result comes before its call.
			call := byID(hookruntest.Named(g.seen, semconv.TermaToolCallEvent))["step-2"]
			result := byID(hookruntest.Named(g.seen, semconv.TermaToolResultEvent))["step-2"]
			if !call.Time.After(result.Time) {
				t.Errorf("the late call at %v, its result at %v: the order changed", call.Time, result.Time)
			}
		})
	}
}

// Every event of a conversation leaves strictly later than the one stamped before it, the
// hooks' own included, and each result after its call; two PostToolUse hooks at once too.
func TestAntigravityEventsLeaveInOrder(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	g.drain()
	// The session start sorts before the first turn's prompt.
	if len(g.seen) < 2 || g.seen[0].Name != semconv.TermaSessionStartEvent || g.seen[1].Name != semconv.TermaUserMessageEvent {
		t.Errorf("the conversation opens with %v", hookruntest.Names(g.seen))
	}
	var last time.Time
	for _, e := range g.seen {
		if !e.Time.After(last) {
			t.Errorf("%s at %v, not after %v", e.Name, e.Time, last)
		}
		last = e.Time
	}

	// Two calls of a parallel batch whose hooks run at once.
	p := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	batch := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:47Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"tool_calls":[{"name":"run_command","args":{"CommandLine":"made-up"}},{"name":"run_command","args":{"CommandLine":"made-up"}}]}`
	p.lines = []string{p.lines[0], batch}
	p.write(1)
	p.hook(preInvocation, begin("1"))
	p.write(2)
	var wg sync.WaitGroup
	for _, step := range []int{2, 3} {
		wg.Go(func() { p.hook(postToolUse, toolUse(step, "run_command", `{"CommandLine":"made-up"}`)) })
	}
	wg.Wait()
	p.drain()
	seen := map[int64]bool{}
	for _, e := range p.seen {
		ms := e.Time.UnixMilli()
		if seen[ms] {
			t.Errorf("two events at %v", e.Time)
		}
		seen[ms] = true
	}
	if n := len(hookruntest.Named(p.seen, semconv.TermaToolCallEvent)); n != 2 {
		t.Errorf("%d calls of two hooks at once", n)
	}
}
