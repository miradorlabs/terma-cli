package copilot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The payloads below are the Copilot runtime's own (CLI 1.0.96, alone and inside VS Code
// 1.141), with the ids and paths changed.
const copilotSession = "09748795-2e7e-4e23-b449-81fce5ede88b"

func payload(event, cwd, rest string) string {
	if rest != "" {
		rest = "," + rest
	}
	return `{"hook_event_name":"` + event + `","session_id":"` + copilotSession + `","timestamp":"2026-10-13T14:58:27.831Z","cwd":"` + hookruntest.InJSON(cwd) + `"` + rest + `}`
}

// The session a hook announces is the one stamped on the commit of the files its edit
// tools wrote, whether the tool named them in a path or in an apply_patch envelope.
func TestCopilotSessionIsStampedOnItsCommit(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	ctx := context.Background()
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: t.TempDir(), Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	// The CLI submits the prompt before it starts the session.
	if err := prompt(ctx, env(payload("UserPromptSubmit", root, `"prompt":"add two files"`))); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(payload("SessionStart", root, `"source":"new","initial_prompt":"add two files"`))); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")
	written := `"tool_name":"Write","tool_input":{"path":"` + hookruntest.InJSON(filepath.Join(root, "src", "a.go")) + `","file_text":"package src\n"},"tool_result":{"result_type":"success","text_result_for_llm":"Created file"}`
	patched := `"tool_name":"Edit","tool_input":"*** Begin Patch\n*** Add File: src/b.go\n+package src\n*** End Patch\n","tool_result":{"result_type":"success","text_result_for_llm":"Added 1 file(s)"}`
	for _, rest := range []string{written, patched} {
		if err := postToolUse(ctx, env(payload("PostToolUse", root, rest))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	commitEnv := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	if err := hookrun.PrepareCommitMsg(ctx, commitEnv); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: "+copilotSession) || !strings.Contains(string(data), "Agent-Tool: copilot") {
		t.Fatalf("commit not stamped for Copilot:\n%s", data)
	}
	if err := stop(ctx, env(payload("Stop", root, `"stop_reason":"end_turn","stop_hook_active":false`))); err != nil {
		t.Fatal(err)
	}
	if err := sessionEnd(ctx, env(payload("SessionEnd", root, `"reason":"complete"`))); err != nil {
		t.Fatal(err)
	}
	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if got := hookruntest.Names(events); len(hookruntest.Named(events, semconv.TermaFilesTouchedEvent)) != 2 {
		t.Fatalf("expected a start, two edits and an end in the spool, got %s", got)
	}
}

// Only a tool that wrote, and whose runtime did not report a failure, records a file: a
// read, a shell command and a refused edit record none.
func TestCopilotRecordsOnlyEditsThatHappened(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	ctx := context.Background()
	file := hookruntest.InJSON(filepath.Join(root, "a.go"))
	for _, rest := range []string{
		`"tool_name":"Read","tool_input":{"path":"` + file + `","view_range":[1,200]},"tool_result":{"result_type":"success","text_result_for_llm":"x"}`,
		`"tool_name":"Bash","tool_input":{"command":"printf ok > a.go","description":"write"},"tool_result":{"result_type":"success","text_result_for_llm":""}`,
		`"tool_name":"Edit","tool_input":{"path":"` + file + `","old_str":"a","new_str":"b"},"tool_result":{"result_type":"failure","text_result_for_llm":"no match"}`,
		`"tool_name":"Edit","tool_input":{"old_str":"a","new_str":"b"},"tool_result":{"result_type":"success","text_result_for_llm":""}`,
	} {
		env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(payload("PostToolUse", root, rest)), Spool: sp, Version: "test"}
		if err := postToolUse(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	if touched := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaFilesTouchedEvent); len(touched) > 0 {
		t.Fatalf("recorded %d edits that did not happen", len(touched))
	}
}

// Each edit tool spells its file differently: the Copilot runtime's path, VS Code's own
// agent's filePath and per-replacement filePath, and an apply_patch envelope as the whole
// input or under "input".
func TestEditedPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"runtime write", `{"path":"/r/note.txt","file_text":"ok"}`, []string{"/r/note.txt"}},
		{"runtime edit", `{"path":"/r/b.txt","old_str":"one","new_str":"two"}`, []string{"/r/b.txt"}},
		{"runtime patch", `"*** Begin Patch\n*** Update File: a.txt\n@@\n-one\n+two\n*** End Patch\n"`, []string{"a.txt"}},
		{"vscode create", `{"filePath":"/r/c.txt","content":"x"}`, []string{"/r/c.txt"}},
		{"vscode replacements", `{"explanation":"x","replacements":[{"filePath":"/r/a.go"},{"filePath":"/r/b.go"},{"filePath":"/r/a.go"}]}`, []string{"/r/a.go", "/r/b.go"}},
		{"vscode patch", `{"input":"*** Begin Patch\n*** Update File: /r/a.go\n*** Move to: /r/z.go\n*** End Patch","explanation":"x"}`, []string{"/r/a.go", "/r/z.go"}},
		{"notebook edit", `{"notebook_path":"/r/n.ipynb","new_source":"x"}`, []string{"/r/n.ipynb"}},
		{"no path", `{"old_str":"a"}`, nil},
		{"not an input", `[1,2]`, nil},
		{"nothing", ``, nil},
	} {
		if got := editedPaths([]byte(tc.input)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every handler survives input it cannot understand: a failing hook gets removed.
func TestCopilotHandlersIgnoreBadInput(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ctx := context.Background()
	for _, stdin := range []string{"", "not json", `{"hook_event_name":"SessionStart"}`, `{"session_id":"../../etc"}`, `{"session_id":"s1","tool_name":"Edit","tool_input":7}`} {
		for name, h := range (Agent{}).Events() {
			env := hookrun.Env{StateDir: stateDir, Cwd: t.TempDir(), Stdin: strings.NewReader(stdin)}
			if err := h(ctx, env); err != nil {
				t.Errorf("%s(%q) = %v", name, stdin, err)
			}
		}
	}
}

// The hooks file is terma's own beside the developer's: five events by the names both the
// CLI and VS Code answer with one payload, each running terma by its `command`, and gone
// whole once its hooks are.
func TestUserHooksFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := func(event string) string { return "[ -x '/x/terma' ] && '/x/terma' hook --user " + event + " || true" }
	plan, err := planUserHooks(dir, cmd, true)
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Path != "terma.json" {
		t.Fatalf("plan: %+v, %v", plan, err)
	}
	after := string(plan.Changes[0].After)
	for _, want := range []string{`"version": 1`, `"SessionStart"`, `"UserPromptSubmit"`, `"PostToolUse"`, `"Stop"`, `"SessionEnd"`,
		`"command": "[ -x '/x/terma' ] && '/x/terma' hook --user copilot-post-tool-use || true"`, `"timeoutSec": 10`} {
		if !strings.Contains(after, want) {
			t.Errorf("hooks file lacks %s:\n%s", want, after)
		}
	}
	if strings.Contains(after, "PreToolUse") {
		t.Errorf("terma wrote a permission gate:\n%s", after)
	}
	if err := os.WriteFile(filepath.Join(dir, "terma.json"), plan.Changes[0].After, 0o600); err != nil {
		t.Fatal(err)
	}
	removal, err := planUserHooks(dir, cmd, false)
	if err != nil || len(removal.Changes) != 1 || removal.Changes[0].After != nil {
		t.Fatalf("removing terma's hooks should remove terma's file: %+v, %v", removal, err)
	}
}
