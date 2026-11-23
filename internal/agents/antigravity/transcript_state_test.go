package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// callState is the path of the rig's conversation's calls state.
func (g *transcriptRig) callState() string {
	return filepath.Join(g.stateDir, antigravityCallDir, hookrun.EvidenceID(g.id)+".json")
}

func (g *transcriptRig) readCallState() antigravityCallState {
	g.t.Helper()
	b, err := os.ReadFile(g.callState())
	if err != nil {
		g.t.Fatal(err)
	}
	var s antigravityCallState
	if err := json.Unmarshal(b, &s); err != nil {
		g.t.Fatalf("calls state %q: %v", b, err)
	}
	return s
}

// A calls state terma cannot parse leaves what was sent unknown: the hook that finds it
// sends and counts nothing, and the calls written after it go once each.
func TestAntigravityUnparsableCallStateSendsNothingAgain(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(postInvocation, invoked)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn 8 sent %s", s)
	}
	if err := os.WriteFile(g.callState(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.hook(postInvocation, invoked)
	if s := g.calls(); s != "" {
		t.Fatalf("with the state unreadable, sent %s again", s)
	}
	if n := g.lost(); n != 0 {
		t.Fatalf("counted %d lost with what was sent unknown", n)
	}
	if s := g.readCallState(); s.Step != 11 {
		t.Fatalf("the fresh state is at step %d, want 11", s.Step)
	}
	g.lines = g.lines[:12]
	g.add(strings.Replace(call22, `"step_index":22`, `"step_index":12`, 1))
	g.hook(stop, stopped)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-12 turn-8" || g.lost() != 0 {
		t.Fatalf("after the fresh state, sent %q, lost %d", s, g.lost())
	}
}

// doneAfter is a hook's context whose time runs out after n checks.
type doneAfter struct {
	context.Context
	n int
}

func (c *doneAfter) Err() error {
	if c.n <= 0 {
		return context.DeadlineExceeded
	}
	c.n--
	return nil
}

// A hook out of time stops reading where it is, even among the lines before its turn, and
// the next hook goes on from there.
func TestAntigravityDeadlineStopsTheReadBeforeTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(12)
	env := g.env(g.root, stopped)
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beginAntigravityTurn(env, r, &antigravityHookInput{ConversationID: g.id, InitialNumSteps: []byte("8")})
	// The conversation's first read, out of time after five lines.
	captureAntigravityCalls(&doneAfter{Context: context.Background(), n: 5}, env, r, &antigravityHookInput{ConversationID: g.id, TranscriptPath: g.transcript})
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s past the deadline", s)
	}
	var offset int64
	for _, l := range g.lines[:5] {
		offset += int64(len(l))
	}
	if s := g.readCallState(); s.Offset != offset || s.Tail == "" || s.Step != 3 {
		t.Fatalf("saved %+v, want offset %d, step 3", s, offset)
	}

	g.hook(stop, stopped)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("the next hooks sent %s", s)
	}
	// Step 6, of a turn before terma watched, is past where the first read was cut short:
	// the hook that finishes that read counts it no more than the first would have.
	if n := g.lost(); n != 0 {
		t.Fatalf("counted %d lost of the turns before terma watched", n)
	}
	// Caught up, a later turn's calls no hook of theirs read are counted again.
	g.write(17)
	g.hook(preInvocation, begin("18"))
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want turn 13's two calls", n)
	}
}
