package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A batch's taint across hooks: a state that begins mid-batch, the state saved between hooks,
// a save that fails, and a hook that cannot have the conversation's lock.

// A state that begins in the middle of a tainted batch still withholds the rest of it: the
// taint is tracked before the boundary, as the results are.
func TestAntigravityStateBegunMidBatchStillWithholds(t *testing.T) {
	file := excludedFile(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	own := filepath.Join(g.root, "a.txt")
	g.lines = []string{g.lines[0], callLine(1, readCall(file), readCall(own)), resultLine(2), resultLine(3), replyLine(4)}
	g.write(3)
	g.hook(preInvocation, begin("1"))
	if s := g.readCallState(); !s.Tainted || s.Before != 0 {
		t.Fatalf("the begun state is %+v", s)
	}
	g.write(5)
	g.hook(stop, stopped)
	got := g.drain()
	if len(got[semconv.TermaToolResultEvent]) != 0 {
		t.Errorf("results of the tainted batch went: %v", got[semconv.TermaToolResultEvent])
	}
	if !g.spoke(4) {
		t.Error("the reply after the batch did not go")
	}
}

// The taint is saved with the state: a hook that reads the call line and the next that reads
// its result agree.
func TestAntigravityTaintSurvivesAHookBoundary(t *testing.T) {
	file := excludedFile(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.lines[1] = withCalls(g.lines[1], readCall(file))
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postInvocation, invoked)
	if s := g.readCallState(); !s.Tainted || s.Step != 1 {
		t.Fatalf("saved %+v", s)
	}
	g.write(4)
	g.hook(postInvocation, invoked)
	got := g.drain()
	if _, result := stepContent(got, "step-2"); result {
		t.Error("the tainted batch's result went in the next hook")
	}
	if s := g.readCallState(); s.Tainted {
		t.Errorf("the next call line left the taint: %+v", s)
	}
}

// A hook whose state cannot be saved moves no cursor, so the next hook reads the call line
// again and still withholds its result.
func TestAntigravityFailedStateWriteLetsNoResultThrough(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only folder does not stop this user writing")
	}
	file := excludedFile(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.lines[1] = withCalls(g.lines[1], readCall(file))
	g.write(1)
	g.hook(preInvocation, begin("1"))
	before := g.readCallState()
	dir := filepath.Dir(g.callState())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	g.write(2)
	g.hook(postInvocation, invoked)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if s := g.readCallState(); s.Offset != before.Offset || s.Tainted {
		t.Fatalf("a state that could not be saved moved: %+v, was %+v", s, before)
	}
	g.write(4)
	g.hook(stop, stopped)
	if _, result := stepContent(g.drain(), "step-2"); result {
		t.Error("the result went after a failed state write")
	}
}

// A hook that cannot have the conversation's lock sends its call as today, with no
// arguments, and leaves the turn standing: the reader of the next hook withholds a refused
// call's result from the transcript alone.
func TestAntigravityHookWithoutTheLockSendsNoContent(t *testing.T) {
	file := excludedFile(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.lines[3] = withCalls(g.lines[3], readCall(file))
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	unlock, err := flock.Lock(context.Background(), g.callState()+".lock")
	if err != nil {
		t.Fatal(err)
	}
	g.hook(postToolUse, toolUse(2, "write_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(g.root, "a.txt"))+`","Content":"made-up"}`))
	g.write(4)
	g.hook(postToolUse, toolUse(4, "read_file", `{"Path":"`+hookruntest.InJSON(file)+`"}`))
	unlock()
	if args, _ := stepContent(g.drain(), "step-2"); args {
		t.Error("arguments went without the lock")
	}
	if antigravityTurnBegun(g.env(g.root, ""), g.id) == nil {
		t.Fatal("a hook without the lock ended the turn")
	}
	g.write(7)
	g.hook(stop, stopped)
	got := g.drain()
	if _, result := stepContent(got, "step-4"); result {
		t.Error("the refused call's result went")
	}
	if _, result := stepContent(got, "step-2"); !result || !g.spoke(6) {
		t.Errorf("the rest of the turn did not go: %v", got)
	}
}
