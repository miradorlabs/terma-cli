package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Content of another repository (design 3.3): a read of a file in a checkout the team does
// not collect sends neither its arguments nor its result; a conversation whose workspaces
// are not all collected sends no content at all.

// readTurn runs turn one with its step 2 a read of path, and the rest as turnOne.
func (g *transcriptRig) readTurn(path string) {
	g.t.Helper()
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "read_file", `{"Path":"`+hookruntest.InJSON(path)+`"}`))
	g.write(4)
	g.hook(postToolUse, toolUse(4, "run_command", `{"CommandLine":"made-up command"}`))
	g.write(7)
	g.hook(postInvocation, invoked)
	g.hook(stop, stopped)
}

// stepContent reports whether step 2's call went with its arguments and its result went.
func stepContent(got map[string][]spool.Event, id string) (args, result bool) {
	_, args = byID(got[semconv.TermaToolCallEvent])[id].Attrs[semconv.GenAIToolCallArgumentsKey]
	_, result = byID(got[semconv.TermaToolResultEvent])[id]
	return args, result
}

func TestAntigravityRefusedReadSendsNeitherArgumentsNorResult(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	other := hookruntest.InitRepo(t)
	none, _ := filepath.EvalSymlinks(t.TempDir())
	for _, dir := range []string{excluded, other, none} {
		hookruntest.WriteFile(t, dir, "file.txt", "made-up\n")
	}
	for _, tc := range []struct {
		name  string
		path  func(g *transcriptRig) string
		shown bool
	}{
		{"an excluded checkout", func(*transcriptRig) string { return filepath.Join(excluded, "file.txt") }, false},
		{"through a link into one", func(g *transcriptRig) string {
			link := filepath.Join(g.root, "linked")
			if err := os.Symlink(excluded, link); err != nil {
				t.Skip("no symlinks here")
			}
			return filepath.Join(link, "file.txt")
		}, false},
		{"an excluded nested clone", func(g *transcriptRig) string {
			nested := filepath.Join(g.root, "nested")
			if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"HEAD", "config"} {
				body := "ref: refs/heads/main\n"
				if f == "config" {
					body = "[remote \"origin\"]\n\turl = https://github.com/acme/elsewhere.git\n"
				}
				hookruntest.WriteFile(t, nested, filepath.Join(".git", f), body)
			}
			hookruntest.WriteFile(t, nested, "file.txt", "made-up\n")
			return filepath.Join(nested, "file.txt")
		}, false},
		{"no repository", func(*transcriptRig) string { return filepath.Join(none, "file.txt") }, true},
		{"another collected checkout", func(g *transcriptRig) string {
			g.admitted = append(g.admitted, other)
			return filepath.Join(other, "file.txt")
		}, true},
		{"its own checkout", func(g *transcriptRig) string { return filepath.Join(g.root, "a.txt") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.readTurn(tc.path(g))
			got := g.drain()
			args, result := stepContent(got, "step-2")
			if args != tc.shown || result != tc.shown {
				t.Errorf("arguments %v, result %v, want %v", args, result, tc.shown)
			}
			if _, called := byID(got[semconv.TermaToolCallEvent])["step-2"]; !called {
				t.Error("the refused call did not go at all")
			}
			// The other call's content and the reply still go.
			if a, r := stepContent(got, "step-4"); !a || !r || len(got[semconv.TermaAssistantMessageEvent]) != 1 {
				t.Errorf("the rest of the turn: arguments %v, result %v, replies %d", a, r, len(got[semconv.TermaAssistantMessageEvent]))
			}
			if s := g.readCallState(); len(s.Refused) != 0 || s.Withheld != nil {
				t.Errorf("refusals left after the turn: %+v", s)
			}
		})
	}
}

// A file in no repository is shown, or refused, by the package's answer.
func TestAntigravityFileInNoRepositoryFollowsTheAnswer(t *testing.T) {
	none, _ := filepath.EvalSymlinks(t.TempDir())
	hookruntest.WriteFile(t, none, "note.md", "made-up\n")
	defer func(was bool) { antigravityShowsFilesInNoRepository = was }(antigravityShowsFilesInNoRepository)
	for _, shows := range []bool{true, false} {
		antigravityShowsFilesInNoRepository = shows
		g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
		g.readTurn(filepath.Join(none, "note.md"))
		if args, result := stepContent(g.drain(), "step-2"); args != shows || result != shows {
			t.Errorf("shows %v: arguments %v, result %v", shows, args, result)
		}
	}
}

// After a refusal the turn's replies go, unless the package's answer withholds them until
// the turn's record ends; the next turn's go either way.
func TestAntigravityRepliesAfterARefusalFollowTheAnswer(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, excluded, "file.txt", "made-up\n")
	defer func(was bool) { antigravityWithholdsRepliesAfterRefusal = was }(antigravityWithholdsRepliesAfterRefusal)
	for _, withholds := range []bool{false, true} {
		antigravityWithholdsRepliesAfterRefusal = withholds
		g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
		g.readTurn(filepath.Join(excluded, "file.txt"))
		g.write(8)
		g.hook(preInvocation, begin("8"))
		g.write(12)
		g.hook(stop, stopped)
		g.drain()
		if g.spoke(6) == withholds || !g.spoke(11) {
			t.Errorf("withholds %v: the refused turn's reply went %v, the next turn's %v", withholds, g.spoke(6), g.spoke(11))
		}
	}
}

// A conversation with a workspace the policy does not collect, resumed from another
// repository or from outside git, sends no content of any kind; its prompt still counts.
func TestAntigravityWorkspaceElsewhereSendsNoContent(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	none, _ := filepath.EvalSymlinks(t.TempDir())
	for name, second := range map[string]string{"an excluded repository": excluded, "outside git": none} {
		t.Run(name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.workspaces = []string{g.root, second}
			g.turnOne()
			got := g.drain()
			if len(got[semconv.TermaModelCallEvent]) != 3 || len(got[semconv.TermaToolCallEvent]) != 2 || len(got[semconv.TermaUserMessageEvent]) != 1 {
				t.Fatalf("what goes today did not: %v", got)
			}
			if _, text := got[semconv.TermaUserMessageEvent][0].Attrs[semconv.TermaMessageTextKey]; text ||
				len(got[semconv.TermaAssistantMessageEvent]) != 0 || len(got[semconv.TermaToolResultEvent]) != 0 {
				t.Errorf("content left: %v", got)
			}
			for _, c := range got[semconv.TermaToolCallEvent] {
				if _, args := c.Attrs[semconv.GenAIToolCallArgumentsKey]; args {
					t.Errorf("arguments left: %v", c.Attrs)
				}
			}
		})
	}
}

// A tool whose hook runs after its result was read (agy's schedule) has its result sent: no
// hook refused it first.
func TestAntigravityResultReadBeforeItsHookIsSent(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(4)
	g.hook(postInvocation, invoked)
	g.hook(postToolUse, toolUse(2, "schedule", `{"Delay":"made-up"}`))
	if args, result := stepContent(g.drain(), "step-2"); !args || !result {
		t.Errorf("arguments %v, result %v", args, result)
	}
}

// A batch of parallel calls with one cancelled keeps each result with its own call: the
// refused one's result is withheld, the other's sent, under the steps the hooks named.
func TestAntigravityParallelCallsKeepTheirResults(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, excluded, "file.txt", "made-up\n")
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	batch := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:47Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"tool_calls":[{"name":"read_file","args":{}},{"name":"read_file","args":{}},{"name":"read_file","args":{}}]}`
	result := func(step string) string {
		return `{"step_index":` + step + `,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T16:51:48Z","content":"<scrubbed>"}`
	}
	reply := `{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:49Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"content":"<scrubbed>"}`
	g.lines = []string{g.lines[0], batch, result("2"), result("3"), reply}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "read_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(excluded, "file.txt"))+`"}`))
	g.hook(postToolUse, toolUse(3, "read_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(g.root, "a.txt"))+`"}`))
	// The third call was cancelled: no hook, no result.
	g.write(5)
	g.hook(stop, stopped)
	got := g.drain()
	if args, result := stepContent(got, "step-2"); args || result {
		t.Errorf("the refused call: arguments %v, result %v", args, result)
	}
	if args, result := stepContent(got, "step-3"); !args || !result {
		t.Errorf("the other call: arguments %v, result %v", args, result)
	}
	if r := byID(got[semconv.TermaToolResultEvent])["step-3"]; r.Attrs[semconv.GenAIToolCallResultKey] != "Created At: 2026-11-20T16:51:50Z\nmade-up result 3" {
		t.Errorf("step 3's result: %v", r.Attrs)
	}
	if s := g.readCallState(); len(s.Refused) != 0 {
		t.Errorf("refused steps after Stop: %v", s.Refused)
	}
}

// A refusal whose result never came (a cancelled call) is cleared at the turn's Stop.
func TestAntigravityRefusalsAreClearedAtStop(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "read_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(excluded, "gone.txt"))+`"}`))
	if s := g.readCallState(); !slices.Equal(s.Refused, []int64{2}) {
		t.Fatalf("refused %v, want step 2", s.Refused)
	}
	g.hook(stop, stopped)
	if s := g.readCallState(); len(s.Refused) != 0 {
		t.Fatalf("refused %v after Stop", s.Refused)
	}
}

// A hook that cannot have the conversation's lock sends its call as today, with no
// arguments; one whose call had to be refused ends the turn, so the result never goes.
func TestAntigravityHookWithoutTheLockSendsNoContent(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	unlock, err := flock.Lock(context.Background(), g.callState()+".lock")
	if err != nil {
		t.Fatal(err)
	}
	g.hook(postToolUse, toolUse(2, "write_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(g.root, "a.txt"))+`","Content":"made-up"}`))
	if args, _ := stepContent(g.drain(), "step-2"); args {
		t.Error("arguments went without the lock")
	}
	if antigravityRecordedTurn(g.env(g.root, ""), g.id) != "turn-1" {
		t.Fatal("a call with nothing to refuse ended the turn")
	}
	g.hook(postToolUse, toolUse(4, "read_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(excluded, "file.txt"))+`"}`))
	unlock()
	if antigravityRecordedTurn(g.env(g.root, ""), g.id) != "" {
		t.Fatal("a refusal that could not be recorded left the turn standing")
	}
	g.write(7)
	g.hook(stop, stopped)
	got := g.drain()
	if _, result := stepContent(got, "step-4"); result || g.spoke(6) {
		t.Errorf("the turn went on after an unrecorded refusal: %v", got)
	}
}

// Every event of a conversation leaves strictly later than the one stamped before it, the
// hooks' own included, and each result after its call; two PostToolUse hooks at once too.
func TestAntigravityEventsLeaveInOrder(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.turnOne()
	g.drain()
	var last time.Time
	for _, e := range g.seen {
		if !e.Time.After(last) {
			t.Errorf("%s at %v, not after %v", e.Name, e.Time, last)
		}
		last = e.Time
	}

	// Two calls of a parallel batch whose hooks run at once.
	p := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	batch := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:47Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"tool_calls":[{"name":"run_command","args":{}},{"name":"run_command","args":{}}]}`
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
