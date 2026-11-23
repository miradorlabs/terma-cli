package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A Stop ends the turn it found when it started, and no other: a turn that began while the
// hook ran keeps its record.
func TestAntigravityStopEndsOnlyTheTurnItFound(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	env := g.env(g.root, "")
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := antigravityTurnBegun(env, g.id)
	g.write(13)
	g.hook(preInvocation, begin("13"))
	endAntigravityTurnBegun(env, g.id, found)
	if turn := antigravityTurnID(env, r, g.id); turn != "turn-13" {
		t.Fatalf("turn 8's Stop left %q, want turn 13's record", turn)
	}
	endAntigravityTurnBegun(env, g.id, antigravityTurnBegun(env, g.id))
	if turn := antigravityTurnID(env, r, g.id); turn != "" {
		t.Fatalf("turn 13's own Stop left %q", turn)
	}
}

// A subagent's link a hook passes is not a model call: only the calls are counted lost, and
// the link is never sent as another turn's.
func TestAntigravityPassedSubagentLinkIsNotCountedAsACall(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	// The turn is cancelled after its second call: no hook read steps 1 to 3.
	g.write(4)
	g.hook(preInvocation, begin("5"))
	got := g.drain()
	if calls := got[semconv.TermaModelCallEvent]; len(calls) != 0 {
		t.Fatalf("the next turn sent the cancelled turn's calls: %v", calls)
	}
	for _, e := range got[semconv.TermaSessionStartEvent] {
		if e.SessionID == childConversation {
			t.Fatalf("the next turn sent the cancelled turn's subagent link: %v", e)
		}
	}
	if n := g.lost(); n != 2 {
		t.Errorf("counted %d lost, want the two calls and not the subagent's link", n)
	}
}

// Every hook where the policy collects nothing ends the turn, a PreInvocation in the middle
// of it included: the calls made after it never leave from a hook back where it began.
func TestAntigravityMidTurnInvocationWhereExcludedEndsTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(10)
	g.hookAt(preInvocation, hookruntest.InitRepo(t), `"initialNumSteps":10,"invocationNum":1`)
	if got := g.drain(); len(got[semconv.TermaModelCallEvent]) != 0 {
		t.Fatalf("the excluded hook spooled %v", got)
	}
	g.write(12)
	g.hook(postInvocation, `"initialNumSteps":10,"invocationNum":1`)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s after a hook of the turn ran where the policy collects nothing", s)
	}
}

// A hook where the policy collects nothing reads nothing and writes no calls state: a
// conversation only ever seen there leaves none, and one followed in a collected checkout
// keeps its state as it was.
func TestAntigravityHookWhereExcludedReadsAndWritesNothing(t *testing.T) {
	hooks := func(g *transcriptRig, at string) {
		t.Helper()
		g.hookAt(preInvocation, at, begin("13"))
		g.write(17)
		g.hookAt(postToolUse, at, `"stepIdx":15,"toolCall":{"name":"read_file","args":{}}`)
		g.hookAt(postInvocation, at, invoked)
		g.hookAt(stop, at, stopped)
		if got := g.drain(); len(got) != 0 {
			t.Fatalf("the excluded hooks spooled %v", got)
		}
	}
	t.Run("only ever excluded", func(t *testing.T) {
		g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
		g.write(13)
		hooks(g, hookruntest.InitRepo(t))
		for _, p := range []string{g.callState(), g.callState() + ".lock", antigravityTurnPath(g.stateDir, g.id)} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s: %v, want none", filepath.Base(p), err)
			}
		}
	})
	t.Run("followed before", func(t *testing.T) {
		g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
		g.write(8)
		g.hook(preInvocation, begin("8"))
		g.write(12)
		g.hook(stop, stopped)
		if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
			t.Fatalf("turn 8 sent %s", s)
		}
		before, err := os.ReadFile(g.callState())
		if err != nil {
			t.Fatal(err)
		}
		g.write(13)
		hooks(g, hookruntest.InitRepo(t))
		if after, _ := os.ReadFile(g.callState()); !bytes.Equal(after, before) {
			t.Fatalf("the excluded hooks changed the state to %s, was %s", after, before)
		}
	})
}

// A PostInvocation at invocation 0 carries its turn's first step: a record naming another
// turn is stale, since this turn's first hook never ran. It ends that record, sends nothing
// as the old turn's and counts what it passes. A PostInvocation at invocation 0 of the
// record's own turn sends as usual (below, and TestAntigravityCallsGoOnceEachWithTheirTokens).
func TestAntigravityPostInvocationZeroEndsAStaleRecord(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(18)
	g.hook(preInvocation, begin("18"))
	// Turn 18's call lands, and the turn is cancelled: no hook of it runs again.
	g.write(21)
	// Turn 22's first hook never ran.
	g.add(userInput21, call22)
	g.hook(postInvocation, begin("22"))
	if s := g.calls(); s != "" {
		t.Fatalf("a stale record sent %s", s)
	}
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want steps 19 and 22", n)
	}
	g.hook(stop, stopped)
	if s := g.calls(); s != "" || g.lost() != 2 {
		t.Fatalf("the Stop after sent %q, lost %d", s, g.lost())
	}
	// A turn whose first hook ran: its PostInvocation at invocation 0 keeps the record.
	g.add(userInput23)
	g.hook(preInvocation, begin("24"))
	g.add(call24)
	g.hook(postInvocation, begin("24"))
	if s := g.calls(); s != "step-24 turn-24" {
		t.Fatalf("turn 24 sent %s", s)
	}
}
