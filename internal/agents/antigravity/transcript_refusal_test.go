package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
// are not all collected sends no content at all. The hook judges its own call's arguments;
// the reader judges the call line's copy and withholds every result of a batch that asked
// for a refused call (a tainted batch).

// readCall is a tool call of a call line: read_file of path.
func readCall(path string) string {
	return `{"name":"read_file","args":{"Path":"` + hookruntest.InJSON(path) + `"}}`
}

// withCalls is a fixture's call line asking for calls, each a JSON tool call.
func withCalls(line string, calls ...string) string {
	i := strings.Index(line, `"tool_calls":`)
	if i < 0 {
		return strings.TrimSuffix(strings.TrimSpace(line), "}") + `,"tool_calls":[` + strings.Join(calls, ",") + "]}"
	}
	return line[:i] + `"tool_calls":[` + strings.Join(calls, ",") + "]}"
}

// callLine is a call line at step asking for calls, with no reply.
func callLine(step int, calls ...string) string {
	return withCalls(`{"step_index":`+strconv.Itoa(step)+`,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:47Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1}`, calls...)
}

// resultLine is a tool's result at step, filled with made-up text.
func resultLine(step int) string {
	return `{"step_index":` + strconv.Itoa(step) + `,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T16:51:48Z","content":"<scrubbed>"}`
}

// replyLine is a call line at step with a reply and no calls.
func replyLine(step int) string {
	return `{"step_index":` + strconv.Itoa(step) + `,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T16:51:49Z","model":"gemini-3.8-flash-low","input_tokens":5,"cache_read_tokens":0,"output_tokens":1,"content":"<scrubbed>"}`
}

// readTurn runs turn one with its step 2 a read of path, and the rest as turnOne.
func (g *transcriptRig) readTurn(path string) {
	g.t.Helper()
	g.lines[1] = withCalls(g.lines[1], readCall(path))
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

// excludedFile is a file in a checkout the rig's policy does not collect.
func excludedFile(t *testing.T) string {
	t.Helper()
	excluded := hookruntest.InitRepo(t)
	hookruntest.WriteFile(t, excluded, "file.txt", "made-up\n")
	return filepath.Join(excluded, "file.txt")
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
			// The next batch's content and the reply still go.
			if a, r := stepContent(got, "step-4"); !a || !r || len(got[semconv.TermaAssistantMessageEvent]) != 1 {
				t.Errorf("the rest of the turn: arguments %v, result %v, replies %d", a, r, len(got[semconv.TermaAssistantMessageEvent]))
			}
			if s := g.readCallState(); s.Tainted || s.WithholdsFrom != nil {
				t.Errorf("a taint left after the turn: %+v", s)
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

// After a tainted batch the turn's replies go, unless the package's answer withholds them
// until the turn's record ends; the next turn's go either way.
func TestAntigravityRepliesAfterARefusalFollowTheAnswer(t *testing.T) {
	file := excludedFile(t)
	defer func(was bool) { antigravityWithholdsRepliesAfterRefusal = was }(antigravityWithholdsRepliesAfterRefusal)
	for _, withholds := range []bool{false, true} {
		antigravityWithholdsRepliesAfterRefusal = withholds
		g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
		g.readTurn(file)
		g.write(8)
		g.hook(preInvocation, begin("8"))
		g.write(12)
		g.hook(stop, stopped)
		g.drain()
		if g.spoke(6) == withholds || !g.spoke(11) {
			t.Errorf("withholds %v: the refused turn's reply went %v, the next turn's %v", withholds, g.spoke(6), g.spoke(11))
		}
		if s := g.readCallState(); (s.WithholdsFrom != nil) != withholds {
			t.Errorf("withholds %v: the state keeps %+v", withholds, s)
		}

		// A turn whose first call line cannot be read or placed: its reply follows the answer
		// too, though the last step handled before it is the turn's prompt.
		for name, first := range map[string]string{
			"too long":   strings.Replace(replyLine(1), `"content":"<scrubbed>"`, `"content":"`+strings.Repeat("x", antigravityLineLimit)+`"`, 1),
			"unreadable": `{"step_index":1,"type":"PLANNER_RESPONSE","tool_calls":[`,
			"no step":    strings.Replace(callLine(1, readCall(filepath.Join(t.TempDir(), "made-up.txt"))), `"step_index":1,`, "", 1),
		} {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.lines = []string{g.lines[0], first, resultLine(2), replyLine(3)}
			g.write(1)
			g.hook(preInvocation, begin("1"))
			g.write(4)
			g.hook(stop, stopped)
			g.drain()
			if g.spoke(3) == withholds {
				t.Errorf("withholds %v, a first call line %s: the turn's reply went %v", withholds, name, g.spoke(3))
			}
		}
	}
}

// A conversation with a workspace the policy does not collect, resumed from another
// repository or from outside git, sends no content of any kind; its prompt still counts.
func TestAntigravityWorkspaceElsewhereSendsNoContent(t *testing.T) {
	excluded := hookruntest.InitRepo(t)
	none, _ := filepath.EvalSymlinks(t.TempDir())
	for name, second := range map[string]func(root string) string{
		"an excluded repository": func(string) string { return excluded },
		"outside git":            func(string) string { return none },
		// A removed clone that was nested in the collected checkout.
		"a folder that does not exist": func(root string) string { return filepath.Join(root, "vendor", "removed") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			g.workspaces = []string{g.root, second(g.root)}
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

// A batch of parallel calls, one refused: every result of the batch is withheld, the
// siblings' too, with no pairing by position; each hook still judges its own arguments, and
// the next batch's result goes.
func TestAntigravityTaintedBatchWithholdsEveryResult(t *testing.T) {
	file := excludedFile(t)
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	own := filepath.Join(g.root, "a.txt")
	g.lines = []string{g.lines[0], callLine(1, readCall(file), readCall(own), readCall(own)), resultLine(2), resultLine(3),
		callLine(4, `{"name":"run_command","args":{"CommandLine":"made-up"}}`), resultLine(5), replyLine(6)}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(2)
	g.hook(postToolUse, toolUse(2, "read_file", `{"Path":"`+hookruntest.InJSON(file)+`"}`))
	g.write(3)
	g.hook(postToolUse, toolUse(3, "read_file", `{"Path":"`+hookruntest.InJSON(own)+`"}`))
	// The third call was cancelled: no hook, no result.
	g.write(6)
	g.hook(postToolUse, toolUse(5, "run_command", `{"CommandLine":"made-up"}`))
	g.write(7)
	g.hook(stop, stopped)
	got := g.drain()
	if args, result := stepContent(got, "step-2"); args || result {
		t.Errorf("the refused call: arguments %v, result %v", args, result)
	}
	if args, result := stepContent(got, "step-3"); !args || result {
		t.Errorf("its sibling: arguments %v (want its own), result %v (want withheld)", args, result)
	}
	if args, result := stepContent(got, "step-5"); !args || !result {
		t.Errorf("the next batch: arguments %v, result %v", args, result)
	}
	if !g.spoke(6) {
		t.Error("the reply after the batch did not go")
	}
}

// A call line too long to read taints its batch: its calls are unknown.
func TestAntigravityOverlongCallLineTaints(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	long := strings.Replace(replyLine(1), `"content":"<scrubbed>"`, `"content":"`+strings.Repeat("x", antigravityLineLimit)+`"`, 1)
	g.lines = []string{g.lines[0], long, resultLine(2), callLine(3, `{"name":"run_command","args":{}}`), resultLine(4)}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(3)
	g.hook(postInvocation, invoked)
	if s := g.readCallState(); !s.Tainted {
		t.Fatalf("after an overlong line, the state is %+v", s)
	}
	g.write(5)
	g.hook(stop, stopped)
	got := g.drain()
	if _, result := stepContent(got, "step-2"); result {
		t.Error("a result after an overlong line went")
	}
	if _, result := stepContent(got, "step-4"); !result {
		t.Error("the next batch's result was withheld")
	}
}

// A call line whose step is missing or negative cannot be placed: its calls are unknown, so it
// taints until the next call line, though the batch before it was clean.
func TestAntigravityCallLineWithNoStepTaints(t *testing.T) {
	for name, edit := range map[string]string{"missing": "", "negative": `"step_index":-1,`} {
		t.Run(name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			own := filepath.Join(g.root, "a.txt")
			stepless := strings.Replace(callLine(3, readCall(own)), `"step_index":3,`, edit, 1)
			g.lines = []string{g.lines[0], callLine(1, readCall(own)), resultLine(2), stepless, resultLine(3),
				callLine(4, `{"name":"run_command","args":{"CommandLine":"made-up"}}`), resultLine(5), replyLine(6)}
			g.write(1)
			g.hook(preInvocation, begin("1"))
			g.write(5)
			g.hook(postInvocation, invoked)
			if s := g.readCallState(); !s.Tainted {
				t.Fatalf("after a call line with no step, the state is %+v", s)
			}
			g.write(8)
			g.hook(stop, stopped)
			got := g.drain()
			if _, result := stepContent(got, "step-2"); !result {
				t.Error("the clean batch's result before it was withheld")
			}
			if _, result := stepContent(got, "step-3"); result {
				t.Error("a result after a call line with no step went")
			}
			if _, result := stepContent(got, "step-5"); !result {
				t.Error("the next batch's result was withheld")
			}
		})
	}
}

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

// A tainted batch still links the subagent it opened: the link is not content. A folder a
// tool runs in (Cwd) is judged as a file is.
func TestAntigravityTaintedBatchStillLinksItsSubagent(t *testing.T) {
	excluded := filepath.Dir(excludedFile(t))
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	other := strings.ReplaceAll(strings.Replace(g.lines[2], `"step_index":2`, `"step_index":3`, 1), childConversation, turnsConversation)
	g.lines = []string{g.lines[0], withCalls(subagentCall1, `{"name":"invoke_subagent","args":{}}`,
		`{"name":"run_command","args":{"CommandLine":"made-up","Cwd":"`+hookruntest.InJSON(excluded)+`"}}`), g.lines[2], other}
	g.write(1)
	g.hook(preInvocation, begin("1"))
	g.write(4)
	g.hook(stop, stopped)
	got := g.drain()
	if starts := linked(got); len(starts) != 1 || starts[0].SessionID != childConversation {
		t.Errorf("links: %v", starts)
	}
	if len(got[semconv.TermaToolResultEvent]) != 0 {
		t.Errorf("results of the tainted batch went: %v", got[semconv.TermaToolResultEvent])
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
