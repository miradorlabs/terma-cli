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

// A calls state terma cannot parse leaves what was sent unknown: a state begins again, and
// nothing on disk then is sent or counted; the calls written after it go once each.
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

// The state is lost in the middle of a turn, unparsable or removed, and the read that
// begins again is cut short before the calls already sent: no later hook sends them again
// or counts them, and a call written after goes once.
func TestAntigravityStateBegunAgainSendsNothingOnDiskThen(t *testing.T) {
	for name, lose := range map[string]func(path string) error{
		"unparsable": func(path string) error { return os.WriteFile(path, []byte("{not json"), 0o600) },
		"removed":    os.Remove,
	} {
		for _, cut := range []bool{true, false} {
			t.Run(name+map[bool]string{true: ", read cut short", false: ""}[cut], func(t *testing.T) {
				g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
				g.write(8)
				g.hook(preInvocation, begin("8"))
				g.write(12)
				g.hook(postInvocation, invoked)
				if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
					t.Fatalf("turn 8 sent %s", s)
				}
				if err := lose(g.callState()); err != nil {
					t.Fatal(err)
				}
				if cut {
					// Out of time before step 9.
					g.readCut(9)
					if s := g.readCallState(); s.Offset != g.size(9) || s.Before != g.size(12) {
						t.Fatalf("saved %+v", s)
					}
				}
				g.hook(postInvocation, invoked)
				if s := g.calls(); s != "" || g.lost() != 0 {
					t.Fatalf("after the state was lost, sent %q, lost %d", s, g.lost())
				}
				g.lines = g.lines[:12]
				g.add(strings.Replace(call22, `"step_index":22`, `"step_index":12`, 1))
				g.hook(stop, stopped)
				g.hook(stop, stopped)
				if s := g.calls(); s != "step-12 turn-8" || g.lost() != 0 {
					t.Fatalf("the call after: sent %q, lost %d", s, g.lost())
				}
			})
		}
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

// readCut runs the conversation's read as a hook of the record's turn would, out of time
// after n lines.
func (g *transcriptRig) readCut(n int) {
	g.t.Helper()
	env := g.env(g.root, stopped)
	r, err := env.Repo(context.Background())
	if err != nil {
		g.t.Fatal(err)
	}
	conv := openAntigravityConversation(context.Background(), env, r, &antigravityHookInput{ConversationID: g.id, TranscriptPath: g.transcript})
	defer conv.close()
	conv.read(&doneAfter{Context: context.Background(), n: n})
}

// size is the bytes of the fixture's first n lines.
func (g *transcriptRig) size(n int) int64 {
	var size int64
	for _, l := range g.lines[:n] {
		size += int64(len(fill(strings.TrimSuffix(l, "\n")) + "\n"))
	}
	return size
}

// A hook out of time stops reading where it is, even among the lines before its turn, and
// the next hook goes on from there. The lines on disk when the state began are never sent or
// counted, however many hooks the read takes; a later turn's unread calls are counted.
func TestAntigravityDeadlineStopsTheReadBeforeTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	env := g.env(g.root, begin("8"))
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Turn 8's PreInvocation: the conversation's first read, out of time after five lines.
	beginAntigravityTurn(env, r, &antigravityHookInput{ConversationID: g.id, InitialNumSteps: []byte("8")})
	g.readCut(5)
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s past the deadline", s)
	}
	// Step 4 is the result of step 3's call: the highest step handled.
	if s := g.readCallState(); s.Offset != g.size(5) || s.Tail == "" || s.Step != 4 || s.Before != g.size(8) {
		t.Fatalf("saved %+v, want offset %d, step 4, before %d", s, g.size(5), g.size(8))
	}

	// The turn's calls are written after its first hook; the next hook finishes the first
	// read, passing step 6 uncounted, and sends them.
	g.write(12)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("the next hooks sent %s", s)
	}
	if n := g.lost(); n != 0 {
		t.Fatalf("counted %d lost of the turns before terma watched", n)
	}
	if s := g.readCallState(); s.Before != 0 {
		t.Fatalf("the boundary is still %d past it", s.Before)
	}
	// A later turn's calls no hook of theirs read are counted.
	g.write(17)
	g.hook(preInvocation, begin("18"))
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want turn 13's two calls", n)
	}
}

// A slow first read hides no loss of a turn begun after the state did: turn 8's first
// read is cut short, turn 8 is cancelled with no hook after its first, and turn 13's first
// hook counts turn 8's calls, and not step 6.
func TestAntigravitySlowFirstReadCountsALaterTurnsLoss(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	env := g.env(g.root, begin("8"))
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beginAntigravityTurn(env, r, &antigravityHookInput{ConversationID: g.id, InitialNumSteps: []byte("8")})
	g.readCut(5)
	g.write(13)
	g.hook(preInvocation, begin("13"))
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s", s)
	}
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want turn 8's two calls", n)
	}
}

// A rewrite before the first read passed the boundary begins it again: nothing the
// rewritten file holds is sent or counted, and a call written after goes.
func TestAntigravityRewriteWhileCatchingUpBeginsTheBoundaryAgain(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(13)
	env := g.env(g.root, begin("13"))
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beginAntigravityTurn(env, r, &antigravityHookInput{ConversationID: g.id, InitialNumSteps: []byte("13")})
	g.readCut(5)
	g.write(17)
	// Compaction: turn 8's prompt to turn 13's last call, shorter than the boundary was.
	g.lines = append([]string{}, g.lines[7:17]...)
	g.write(len(g.lines))
	g.hook(postInvocation, invoked)
	if s := g.calls(); s != "" || g.lost() != 0 {
		t.Fatalf("after the rewrite, sent %q, lost %d", s, g.lost())
	}
	if s := g.readCallState(); s.Before != 0 || s.Step != 16 {
		t.Fatalf("saved %+v, want the boundary passed at step 16", s)
	}
	g.add(strings.Replace(call22, `"step_index":22`, `"step_index":17`, 1))
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-17 turn-13" || g.lost() != 0 {
		t.Fatalf("the call after the rewrite: sent %q, lost %d", s, g.lost())
	}
}
