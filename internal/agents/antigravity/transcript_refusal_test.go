package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Content of another repository (design 3.3), through the transcript's reader: a read of a
// file in a checkout the team does not collect sends neither its arguments nor its result.
// The hook judges its own call's arguments; the reader judges the call line's copy and
// withholds every result of a batch that asked for a refused call (a tainted batch). The
// workspace rule, the taint across hooks, and late hooks and order have a file each beside
// this one (transcript_refusal_*_test.go).

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
			if s := g.readCallState(); s.Tainted {
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

// A tainted batch still links the subagent it opened: the link is not content.
func TestAntigravityTaintedBatchStillLinksItsSubagent(t *testing.T) {
	g := newTranscriptRig(t, parentConversation, "transcript_subagent_parent.jsonl")
	other := strings.ReplaceAll(strings.Replace(g.lines[2], `"step_index":2`, `"step_index":3`, 1), childConversation, turnsConversation)
	g.lines = []string{g.lines[0], withCalls(subagentCall1, `{"name":"invoke_subagent","args":{}}`, readCall(excludedFile(t))), g.lines[2], other}
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

// A decision, not a gap: a batch of tools outside agy's own file and folder tools is never
// tainted by what their arguments name, an argument-less call included, so every result goes,
// as Claude Code's and Codex's do.
func TestAntigravityOtherToolsResultsGoAsClaudeCodesAndCodexsDo(t *testing.T) {
	calls := otherToolCalls(excludedFile(t))
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	g.lines = []string{g.lines[0], callLine(1, calls...)}
	for i := range calls {
		g.lines = append(g.lines, resultLine(2+i))
	}
	g.write(2)
	g.hook(preInvocation, begin("1"))
	g.hook(postToolUse, toolUse(2, "mcp_server_read", string(argsJSON(t, map[string]any{"path": excludedFile(t)}))))
	g.write(len(g.lines))
	g.hook(stop, stopped)
	got := g.drain()
	if args, _ := stepContent(got, "step-2"); !args {
		t.Error("the MCP tool's arguments did not go")
	}
	if n := len(got[semconv.TermaToolResultEvent]); n != len(calls) {
		t.Errorf("%d results went, want %d", n, len(calls))
	}
}
