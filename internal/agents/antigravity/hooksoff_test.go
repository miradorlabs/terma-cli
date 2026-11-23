package antigravity

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// hooksOff runs the handlers agy's hooks reach with hooks off, with the Env the dispatcher
// builds for them: no payload, spool or policy. Each still answers {}.
func hooksOff(t *testing.T, stateDir string, events ...string) {
	t.Helper()
	off := Agent{}.WhenHooksOff()
	for _, event := range events {
		var out bytes.Buffer
		env := hookrun.Env{Now: time.Now(), Stdout: &out, StateDir: stateDir}
		if err := off[event](context.Background(), env); err != nil {
			t.Fatalf("%s with hooks off: %v", event, err)
		}
		if out.String() != "{}\n" {
			t.Fatalf("%s with hooks off answered %q", event, out.String())
		}
	}
}

// The reviewer's sequence: turn 1 is followed, hooks go off while it ends and turn 8
// begins, and come back during turn 8. Nothing of turn 8 leaves as turn 1's; the next
// turn begun with hooks on sends its own calls.
func TestAntigravityHooksOffEndsTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	t.Setenv(antigravityConversationEnv, g.id)
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(7)
	g.hook(postInvocation, invoked)
	if s := g.calls(); s != "step-1 turn-1,step-3 turn-1,step-6 turn-1" {
		t.Fatalf("turn 1 sent %s", s)
	}

	hooksOff(t, g.stateDir, "antigravity-stop", "antigravity-pre-invocation")

	g.write(12)
	g.hook(postInvocation, begin("8"))
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("turn 8, begun with hooks off, sent %s", s)
	}
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want turn 8's two calls", n)
	}

	g.write(13)
	g.hook(preInvocation, begin("13"))
	g.write(17)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-14 turn-13,step-16 turn-13" {
		t.Fatalf("turn 13 sent %s", s)
	}
}

// With no valid conversation in agy's environment, a hook with hooks off only answers.
func TestAntigravityHooksOffWithNoConversationOnlyAnswers(t *testing.T) {
	for _, id := range []string{"", "../not an id"} {
		t.Run(id, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			t.Setenv(antigravityConversationEnv, id)
			g.write(1)
			g.hook(preInvocation, begin("1"))
			hooksOff(t, g.stateDir, "antigravity-stop", "antigravity-pre-invocation", "antigravity-post-tool-use", "antigravity-post-invocation")
			env := g.env(g.root, "")
			r, err := env.Repo(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if turn := antigravityTurnID(env, r, g.id); turn != "turn-1" {
				t.Fatalf("the record is %q, want turn-1 untouched", turn)
			}
		})
	}
}
