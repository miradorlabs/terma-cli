package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Which turn, checkout and sign-in a call goes under: only its own turn's, as the turn's
// first hook recorded them.

// overlongPrompt12 is turn 13's prompt, too long to read.
func overlongPrompt12() string {
	return `{"step_index":12,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T16:52:01Z","content":"` + strings.Repeat("x", antigravityLineLimit) + `"}`
}

// withOverlongTurn13 makes the rig's fixture turns 8 and 13 with turn 13's prompt too long
// to read, and returns the fixture as it was.
func withOverlongTurn13(g *transcriptRig) []string {
	orig := g.lines
	g.lines = append(append([]string{}, orig[:12]...), overlongPrompt12(), orig[13], orig[14], orig[15], orig[16])
	return orig
}

// dropTurn13sPrompt rewrites the transcript as compaction might: turn 8's prompt and calls,
// turn 13's calls, and not turn 13's prompt.
func dropTurn13sPrompt(g *transcriptRig, orig []string) {
	g.lines = append(append([]string{}, orig[7:12]...), orig[13:17]...)
	g.write(len(g.lines))
}

// The reviewer's sequence: turn 13 begins where the policy collects nothing, its prompt too
// long to read, and a rewrite drops that prompt. Nothing of turn 13 ever leaves, whatever
// hook runs after, and the next collected turn's calls go.
func TestAntigravityExcludedTurnNeverLeavesAfterARewrite(t *testing.T) {
	for _, excludedStop := range []bool{true, false} {
		t.Run(map[bool]string{true: "stop where excluded", false: "no hook where excluded after the start"}[excludedStop], func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			excluded := hookruntest.InitRepo(t)
			orig := withOverlongTurn13(g)
			check := func(stage, want string) {
				t.Helper()
				if s := g.calls(); s != want {
					t.Fatalf("%s: sent %q, want %q", stage, s, want)
				}
			}
			g.write(8)
			g.hook(preInvocation, begin("8"))
			g.write(12)
			g.hook(stop, stopped)
			check("turn 8", "step-9 turn-8,step-11 turn-8")

			g.write(13)
			g.hookAt(preInvocation, excluded, begin("13"))
			if got := g.drain(); len(got) != 0 {
				t.Fatalf("the excluded hook spooled %v", got)
			}
			g.write(17)
			dropTurn13sPrompt(g, orig)
			if excludedStop {
				g.hookAt(stop, excluded, stopped)
				if got := g.drain(); len(got) != 0 {
					t.Fatalf("the excluded Stop spooled %v", got)
				}
			}
			// Stray hooks back in the collected checkout, with no turn begun there.
			g.hook(postInvocation, invoked)
			check("stray PostInvocation", "")
			g.hook(stop, stopped)
			check("stray Stop", "")

			g.add(orig[17])
			g.hook(preInvocation, begin("18"))
			g.add(orig[18], orig[19])
			g.hook(stop, stopped)
			check("the next collected turn", "step-19 turn-18")
		})
	}
}

// The same sequence in one collected checkout: turn 13's calls go as turn 13's, never as
// turn 8's, though its prompt was never read and is gone.
func TestAntigravityTurnWithAnUnreadPromptKeepsItsID(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	orig := withOverlongTurn13(g)
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn 8 sent %s", s)
	}
	g.write(13)
	g.hook(preInvocation, begin("13"))
	g.write(17)
	dropTurn13sPrompt(g, orig)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-14 turn-13,step-16 turn-13" {
		t.Fatalf("turn 13 sent %s", s)
	}
	g.add(orig[17])
	g.hook(preInvocation, begin("18"))
	g.add(orig[18], orig[19])
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-19 turn-18" {
		t.Fatalf("turn 18 sent %s", s)
	}
}

// A conversation resumed with no cursor: the turns before terma watched never leave and are
// not counted lost; the turn whose start a hook saw goes.
func TestAntigravityResumedConversationSendsOnlyTheTurnItSawBegin(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(18)
	g.hook(preInvocation, begin("18"))
	if s := g.calls(); s != "" {
		t.Fatalf("earlier turns sent %s", s)
	}
	g.write(20)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-19 turn-18" {
		t.Fatalf("sent %s", s)
	}
	if n := g.lost(); n != 0 {
		t.Fatalf("counted %d lost for the turns before terma watched", n)
	}
}

// A collected turn cancelled after a call no hook read, then a turn where the policy
// collects nothing, then a collected one: the old call never leaves, the new turn's go.
func TestAntigravityCancelledCallNeverLeavesPastAnExcludedTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	excluded := hookruntest.InitRepo(t)
	g.write(18)
	g.hook(preInvocation, begin("18"))
	g.write(21)
	g.add(userInput21)
	g.drain()
	g.hookAt(preInvocation, excluded, begin("22"))
	g.add(call22)
	g.hookAt(stop, excluded, stopped)
	if got := g.drain(); len(got) != 0 {
		t.Fatalf("the excluded hooks spooled %v", got)
	}
	g.add(userInput23)
	g.hook(preInvocation, begin("24"))
	if s := g.calls(); s != "" {
		t.Fatalf("the next collected turn's start sent %s", s)
	}
	g.add(call24)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-24 turn-24" {
		t.Fatalf("sent %s", s)
	}
}

// A hook of a turn begun in another collected checkout sends nothing; a turn begun in the
// second checkout sends only its own calls, under that checkout's repository.
func TestAntigravityTurnBegunElsewhereSendsNothingHere(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	second := hookruntest.InitRepo(t)
	g.admitted = append(g.admitted, second)
	g.write(18)
	g.hook(preInvocation, begin("18"))
	g.write(21)
	g.hookAt(stop, second, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("a hook in another checkout sent %s", s)
	}
	if n := g.lost(); n != 1 {
		t.Fatalf("counted %d lost, want turn 18's call", n)
	}
	g.add(userInput21)
	g.hookAt(preInvocation, second, begin("22"))
	g.add(call22)
	g.hookAt(stop, second, stopped)
	calls := g.drain()[semconv.TermaModelCallEvent]
	if len(calls) != 1 || calls[0].Attrs[semconv.TermaModelCallIDKey] != "step-22" || calls[0].Attrs[semconv.TermaTurnIDKey] != "turn-22" ||
		!strings.HasSuffix(calls[0].Repository.Origin, filepath.Base(second)) {
		t.Fatalf("the second checkout sent %v", calls)
	}
	// Back in the first checkout, turn 22 is not its own.
	g.add(strings.Replace(call22, `"step_index":22`, `"step_index":23`, 1))
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("the first checkout sent %s of a turn begun in the second", s)
	}
}

// A hook of a running turn where the policy collects nothing ends the turn: the rest of it
// never leaves, even from hooks back in the collected checkout.
func TestAntigravityTurnEndsAtAHookWhereThePolicyCollectsNothing(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	excluded := hookruntest.InitRepo(t)
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(10)
	g.drain()
	g.hookAt(postToolUse, excluded, `"stepIdx":10,"toolCall":{"name":"edit_file","args":{}}`)
	if got := g.drain(); len(got) != 0 {
		t.Fatalf("the excluded hook spooled %v", got)
	}
	g.write(12)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("the rest of the turn sent %s", s)
	}
	// The excluded hook read nothing: the next collecting hook passes steps 9 and 11.
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want steps 9 and 11", n)
	}
}

// A call of a turn begun under one sign-in is never sent under another's, nor later under
// the first's once a hook of the other has passed it.
func TestAntigravityCallNeverGoesInAnotherSignInsLane(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	first := g.sp
	g.write(18)
	g.hook(preInvocation, begin("18"))
	g.drain()
	g.write(21)
	otherDir := t.TempDir()
	other, err := spool.Open(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	other.Bind("fedcba9876543210")
	g.sp = other
	g.hook(stop, stopped)
	if got := hookruntest.Named(hookruntest.Spooled(t, other), semconv.TermaModelCallEvent); len(got) != 0 {
		t.Fatalf("turn 18's call went in another sign-in's lane: %v", got)
	}
	if l, _ := spool.ReadLosses(otherDir, time.Now()); l.Counts[sendas.LostTurnEnded] != 1 {
		t.Fatalf("the other lane counted %v", l.Counts)
	}
	g.sp = first
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("turn 18's call went later in its own lane: %s", s)
	}
}

// A hook that cannot take the conversation's lock sends nothing and moves nothing; the next
// sends each call once. Hooks at once send each call once between them.
func TestAntigravityCallsGoOnceUnderTheLock(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	state := filepath.Join(g.stateDir, antigravityCallDir, hookrun.EvidenceID(g.id)+".json")
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.Lock(context.Background(), state+".lock")
	if err != nil {
		t.Fatal(err)
	}
	env := g.env(g.root, stopped)
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	read := captureAntigravityCalls(ctx, env, r, &antigravityHookInput{ConversationID: g.id, TranscriptPath: g.transcript})
	cancel()
	if after, _ := os.ReadFile(state); read || !bytes.Equal(after, before) {
		t.Fatalf("read %v under a held lock; state %s, was %s", read, after, before)
	}
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s under a held lock", s)
	}
	unlock()
	g.hook(stop, stopped)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("after the lock, sent %s", s)
	}

	g.write(13)
	g.hook(preInvocation, begin("13"))
	g.write(17)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() { errs <- stop(context.Background(), g.env(g.root, stopped)) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if s := g.calls(); s != "step-14 turn-13,step-16 turn-13" {
		t.Fatalf("hooks at once sent %s", s)
	}
}

// recordTrip is a hook's context that, at its nth check, lets the next turn begin: as a
// later turn's first hook does, between two lines this hook reads.
type recordTrip struct {
	context.Context
	n    int
	trip func()
}

func (c *recordTrip) Err() error {
	if c.n--; c.n == 0 {
		c.trip()
	}
	return c.Context.Err()
}

// A hook reads the record again before each send: a call read after the next turn began is
// never sent as this hook's turn.
func TestAntigravityRecordIsReadAgainBeforeEachSend(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(17)
	env := g.env(g.root, stopped)
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The check before step 14: the hook reads on from line 8, one check a line, and line i
	// is step i.
	ctx := &recordTrip{Context: context.Background(), n: 7, trip: func() {
		beginAntigravityTurn(env, r, &antigravityHookInput{ConversationID: g.id, InitialNumSteps: []byte("13")})
	}}
	captureAntigravityCalls(ctx, env, r, &antigravityHookInput{ConversationID: g.id, TranscriptPath: g.transcript})
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("sent %s", s)
	}
	if n := g.lost(); n != 2 {
		t.Fatalf("counted %d lost, want steps 14 and 16", n)
	}
}

// With no turn record, a hook sends nothing: terma did not see the turn begin, or its
// first hook gave no initialNumSteps to name it by.
func TestAntigravityNoTurnRecordSendsNothing(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(7)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("sent %s with no turn begun", s)
	}

	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn 8 sent %s", s)
	}
	// Turn 13's first hook names no step: turn 8's record goes, and nothing of turn 13 leaves.
	g.write(13)
	g.hook(preInvocation, `"invocationNum":0`)
	g.write(17)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("a turn with no step sent %s", s)
	}
}

// A prompt that joins a running turn (no new invocation 0) leaves the turn as it was: the
// calls after it still go, under the turn's id.
func TestAntigravityPromptJoinedMidTurnKeepsTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	joined := strings.Replace(g.lines[7], `"step_index":7`, `"step_index":10`, 1)
	g.lines = append(g.lines[:10:10], joined, g.lines[11])
	g.write(len(g.lines))
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("sent %s", s)
	}
}

// Stop ends the turn: a later turn whose first hook never ran (agy starts every prompt and
// every wake at invocation 0) sends nothing as the turn before's.
func TestAntigravityStopEndsTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	g.hook(stop, stopped)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn 8 sent %s", s)
	}
	g.write(17)
	g.hook(postToolUse, `"stepIdx":15,"toolCall":{"name":"read_file","args":{}}`)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("turn 13, which no hook saw begin, sent %s", s)
	}
	if n := g.lost(); n != 2 {
		t.Errorf("counted %d lost, want turn 13's two calls", n)
	}
}

// A payload too large to read, as every hook's is in a turn whose prompt is, ends the turn
// terma was following: a hook of that turn terma can read later sends nothing as the turn
// before's.
func TestAntigravityUnreadablePayloadEndsTheTurn(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(8)
	g.hook(preInvocation, begin("8"))
	g.write(12)
	// Turn 8 is cancelled: no Stop ends it.
	g.hook(postInvocation, invoked)
	if s := g.calls(); s != "step-9 turn-8,step-11 turn-8" {
		t.Fatalf("turn 8 sent %s", s)
	}
	t.Setenv(antigravityConversationEnv, g.id)
	env := g.env(g.root, begin("13"))
	env.Stdin = strings.NewReader(`{"conversationId":"` + g.id + `","initialNumSteps":13,"invocationNum":0,"lastUserInput":"` + strings.Repeat("x", hookrun.MaxInput) + `"}`)
	if err := preInvocation(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	g.write(17)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
	if s := g.calls(); s != "" {
		t.Fatalf("turn 13, whose first hook terma could not read, sent %s", s)
	}
}
