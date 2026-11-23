package antigravity

import (
	"context"
	"testing"

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
