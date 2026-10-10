package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestCodexSessionStartAnnouncesSession(t *testing.T) {
	t.Parallel()
	root := hookruntest.InitRepo(t)
	stateDir := t.TempDir()
	const id = "01a0d0ff-0000-7000-8000-000000000003"
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(`{"session_id":"` + id + `","cwd":` + strconv.Quote(root) + `}`)}
	if err := sessionStart(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	// A trusted hook can announce a repository session without a global exporter.
}

// The project hooks find edited files inside the apply_patch envelope a tool call carries.
func TestCodexSessionStampsItsCommitFromApplyPatch(t *testing.T) {
	root := hookruntest.InitRepo(t)
	stateDir := t.TempDir()
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	const id = "01a08bd5-0487-74b1-9d82-45e619c574fa"

	if err := sessionStart(ctx, env(`{"session_id":"`+id+`","hook_event_name":"SessionStart","cwd":`+strconv.Quote(root)+`,"model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")

	patch := strings.Join([]string{
		"apply_patch <<'PATCH'",
		"*** Begin Patch",
		"*** Update File: src/a.go",
		"@@",
		"-old",
		"+new",
		"*** Add File: src/b.go",
		"+package src",
		"*** End Patch",
		"PATCH",
	}, "\\n")
	if err := postToolUse(ctx, env(`{"session_id":"`+id+`","hook_event_name":"PostToolUse","cwd":`+strconv.Quote(root)+`,"model":"gpt-6","permission_mode":"default","tool_name":"apply_patch","tool_use_id":"call_1","turn_id":"turn_1","transcript_path":null,"tool_response":"ok","tool_input":{"command":"`+patch+`"}}`)); err != nil {
		t.Fatal(err)
	}
	touched := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaFilesTouchedEvent)
	if len(touched) != 1 || touched[0].Attrs[semconv.GenAIToolCallIDKey] != "call_1" {
		t.Fatalf("file touch must carry Codex's tool call id: %+v", touched)
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
	if !strings.Contains(string(data), "Agent-Session-Id: "+id) || !strings.Contains(string(data), "Agent-Tool: codex") {
		t.Fatalf("commit not stamped for Codex:\n%s", data)
	}

	if err := sessionEnd(ctx, env(`{"session_id":"`+id+`","hook_event_name":"SessionEnd","cwd":`+strconv.Quote(root)+`,"reason":"closed","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
}

// A commit of only files the agent wrote through the shell is the session's (#55).
func TestCodexSessionStampsItsCommitFromShellWrite(t *testing.T) {
	root := hookruntest.InitRepo(t)
	stateDir := t.TempDir()
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	const id = "01a08bd5-0487-74b1-9d82-45e619c574fb"
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	command := strconv.Quote("cd src && cat > a.go <<'EOF'\npackage src\nEOF")
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Version: "test",
		Stdin: strings.NewReader(`{"session_id":"` + id + `","hook_event_name":"PostToolUse","cwd":` + strconv.Quote(root) + `,"model":"gpt-6","permission_mode":"default","tool_name":"Bash","tool_use_id":"c1","turn_id":"t1","transcript_path":null,"tool_response":"","tool_input":{"command":` + command + `}}`)}
	if err := postToolUse(ctx, env); err != nil {
		t.Fatal(err)
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
	if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Session-Id: "+id) {
		t.Fatalf("commit of a shell-written file not stamped:\n%s", data)
	}
}

// A patch is not a shell script: a redirect inside its body names no file.
func TestCodexEditedPathsParsesShellOnlyForShellCalls(t *testing.T) {
	t.Parallel()
	in := &codexHookInput{ToolName: "apply_patch", ToolInput: []byte(`{"command":"*** Begin Patch\n*** Add File: x.sh\n+echo a > out.txt\n*** End Patch"}`)}
	if got, _ := codexEditedPaths(in, t.TempDir()); len(got) != 1 || got[0] != "x.sh" {
		t.Fatalf("got %v, want [x.sh]", got)
	}
}

// A shell call that changed nothing leaves no manifest behind.
func TestCodexPostToolUseIgnoresCallsWithoutAPatch(t *testing.T) {
	root := hookruntest.InitRepo(t)
	stateDir := t.TempDir()
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Version: "test",
		Stdin: strings.NewReader(`{"session_id":"01a08bd5-0487-74b1-9d82-45e619c574fa","hook_event_name":"PostToolUse","cwd":` + strconv.Quote(root) + `,"model":"gpt-6","permission_mode":"default","tool_name":"shell","tool_use_id":"c1","turn_id":"t1","transcript_path":null,"tool_response":"","tool_input":{"command":"go test ./..."}}`)}
	if err := postToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("manual work\n"), 0o644)
	commitEnv := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	if err := hookrun.PrepareCommitMsg(ctx, commitEnv); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("a shell command that edited nothing claimed the commit:\n%s", data)
	}
}

func TestApplyPatchPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		command string
		want    []string
	}{
		{"none", "go build ./...", nil},
		{"add", "*** Begin Patch\n*** Add File: a/b.go\n*** End Patch", []string{"a/b.go"}},
		{"update and delete", "*** Update File: x.go\n*** Delete File: y.go", []string{"x.go", "y.go"}},
		// A rename touches both paths, and the commit will carry both.
		{"rename", "*** Update File: old.go\n*** Move to: new.go", []string{"old.go", "new.go"}},
		{"indented heredoc", "  *** Add File: spaced.go  ", []string{"spaced.go"}},
		{"header with no path", "*** Add File:", nil},
		{"paths with spaces", "*** Add File: dir with space/f.go", []string{"dir with space/f.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPatchPaths(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Every handler survives input it cannot understand: a failing hook gets removed.
func TestCodexHooksNeverFailOnBadInput(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ctx := context.Background()
	for _, bad := range []string{"", "{", `{"session_id":""}`, `{"session_id":"../../etc/passwd"}`} {
		for name, fn := range map[string]func(context.Context, hookrun.Env) error{
			"session-start": sessionStart,
			"session-end":   sessionEnd,
			"pre-tool-use":  preToolUse,
			"post-tool-use": postToolUse,
		} {
			env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: t.TempDir(), Stdin: strings.NewReader(bad), Version: "test"}
			if err := fn(ctx, env); err != nil {
				t.Fatalf("%s(%q) = %v, want nil", name, bad, err)
			}
		}
	}
}

// Codex runs a call's PostToolUse after the call, so a commit in the call itself, or one
// after a patch run from the shell (which has no PostToolUse), is stamped only through
// PreToolUse. The commands are the ones Codex 0.160.1 ran in live sessions.
func TestCodexPreToolUseStampsACommitInTheSameCall(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		before        []string    // committed first
		mkdir         string      // the directory the command makes first
		write         [][2]string // what the command writes: path, content
		git           [][]string  // and the git commands it runs before committing
	}{
		{
			name:    "write and commit",
			command: "printf x > pairs/p.txt && git add pairs/p.txt && git commit -m p",
			before:  []string{"pairs/keep.txt"},
			write:   [][2]string{{"pairs/p.txt", "x"}},
		},
		{
			name:    "one command per line",
			command: "printf '%s\\n' 'Terma Sandbox Team' > CONTRIBUTORS\n git add -- CONTRIBUTORS\n git commit -m \"Add CONTRIBUTORS with Terma Sandbox Team\"",
			write:   [][2]string{{"CONTRIBUTORS", "Terma Sandbox Team\n"}},
		},
		{
			name:    "into a directory made first",
			command: "mkdir -p pairs && printf 'seven\\n' > pairs/o7.txt && git add -- pairs/o7.txt && git commit -m 'Add pairs/o7.txt with seven' && git status --short && git show --stat --oneline HEAD",
			write:   [][2]string{{"pairs/o7.txt", "seven\n"}},
		},
		{
			name:    "git mv into a directory made first",
			command: "mkdir -p docs/notes && git mv src/p4_subdir.txt docs/notes/p4_subdir.txt && git diff --cached --stat && git commit -m \"Move p4_subdir note into docs/notes\" && git status --short",
			before:  []string{"src/p4_subdir.txt"},
			mkdir:   "docs/notes",
			git:     [][]string{{"mv", "src/p4_subdir.txt", "docs/notes/p4_subdir.txt"}},
		},
		{
			name:    "patch from the shell",
			command: "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: pairs/b.txt\n+heredoc\n*** End Patch\nPATCH",
			before:  []string{"pairs/keep.txt"},
			write:   [][2]string{{"pairs/b.txt", "heredoc\n"}},
		},
		{
			name:    "patch from the shell that updates, deletes and moves",
			command: "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: pairs/d1.txt\n@@\n-one\n+one updated\n*** Delete File: pairs/d2.txt\n*** Update File: pairs/d3.txt\n*** Move to: pairs/d3moved.txt\n@@\n-three\n+three moved\n*** End Patch\nPATCH",
			before:  []string{"pairs/d1.txt", "pairs/d2.txt", "pairs/d3.txt"},
			git:     [][]string{{"rm", "-q", "pairs/d2.txt"}, {"mv", "pairs/d3.txt", "pairs/d3moved.txt"}},
			write:   [][2]string{{"pairs/d1.txt", "one updated\n"}, {"pairs/d3moved.txt", "three moved\n"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := hookruntest.InitRepo(t)
			stateDir := t.TempDir()
			sp, _ := spool.Open(t.TempDir())
			for _, p := range tc.before {
				hookruntest.WriteFile(t, root, p, "before\n")
			}
			if len(tc.before) > 0 {
				for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "before"}} {
					if _, err := gitx.Git(ctx, root, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			const id = "01a1204e-2470-7e31-859b-486d42353986"
			env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Version: "test",
				Stdin: strings.NewReader(`{"session_id":"` + id + `","turn_id":"01a1204e-26df-7540-ae80-03d0159e9625","transcript_path":null,"cwd":` + strconv.Quote(root) +
					`,"hook_event_name":"PreToolUse","model":"gpt-6.1-sol","permission_mode":"bypassPermissions","tool_name":"Bash","tool_input":{"command":` + strconv.Quote(tc.command) +
					`},"tool_use_id":"exec-1acd400f-cd4d-41b9-a47c-a31aaa337bd5"}`)}
			if err := preToolUse(ctx, env); err != nil {
				t.Fatal(err)
			}
			if tc.mkdir != "" {
				if err := os.MkdirAll(filepath.Join(root, tc.mkdir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range tc.git {
				if _, err := gitx.Git(ctx, root, args...); err != nil {
					t.Fatal(err)
				}
			}
			for _, w := range tc.write {
				hookruntest.WriteFile(t, root, w[0], w[1])
			}
			if _, err := gitx.Git(ctx, root, "add", "-A"); err != nil {
				t.Fatal(err)
			}
			msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
			commitEnv := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
			if err := hookrun.PrepareCommitMsg(ctx, commitEnv); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Session-Id: "+id) {
				t.Fatalf("commit made in the same call not stamped:\n%s", data)
			}
			// The call's PostToolUse reports the files; reporting them here too would count them twice.
			for _, ev := range hookruntest.Spooled(t, sp) {
				if ev.Name == semconv.TermaFilesTouchedEvent {
					t.Fatalf("PreToolUse reported files: %+v", ev)
				}
			}
		})
	}
}

// codexCall runs a shell call's hooks around git: PreToolUse, then run (the call's own
// effect, its commits made through commit), then PostToolUse.
type codexCall struct {
	t        *testing.T
	ctx      context.Context
	root     string
	stateDir string
	sp       *spool.Spool
}

const codexCallSession = "01a1204e-2470-7e31-859b-486d42353986"

func newCodexCall(t *testing.T) *codexCall {
	sp, _ := spool.Open(t.TempDir())
	return &codexCall{t: t, ctx: context.Background(), root: hookruntest.InitRepo(t), stateDir: t.TempDir(), sp: sp}
}

func (c *codexCall) env(stdin string, args ...string) hookrun.Env {
	return hookrun.Env{StateDir: c.stateDir, Now: time.Now(), Cwd: c.root, Policy: hookruntest.Admitting(c.root), Args: args, Stdin: strings.NewReader(stdin), Spool: c.sp, Version: "test"}
}

func (c *codexCall) payload(event, command string) string {
	return `{"session_id":"` + codexCallSession + `","turn_id":"01a1204e-26df-7540-ae80-03d0159e9625","transcript_path":null,"cwd":` + strconv.Quote(c.root) +
		`,"hook_event_name":"` + event + `","model":"gpt-6.1-sol","permission_mode":"bypassPermissions","tool_name":"Bash","tool_input":{"command":` + strconv.Quote(command) +
		`},"tool_use_id":"exec-1acd400f-cd4d-41b9-a47c-a31aaa337bd5","tool_response":""}`
}

func (c *codexCall) git(args ...string) {
	c.t.Helper()
	if _, err := gitx.Git(c.ctx, c.root, args...); err != nil {
		c.t.Fatal(err)
	}
}

// commit commits what is staged as the commit hooks see it, and returns its message.
func (c *codexCall) commit(message string) string {
	c.t.Helper()
	msgPath := filepath.Join(c.t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte(message+"\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(c.ctx, c.env("", msgPath, "message")); err != nil {
		c.t.Fatal(err)
	}
	c.git("commit", "-q", "-F", msgPath)
	if err := hookrun.PostCommit(c.ctx, c.env("")); err != nil {
		c.t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	return string(data)
}

func (c *codexCall) run(command string, effect func()) {
	c.t.Helper()
	if err := preToolUse(c.ctx, c.env(c.payload("PreToolUse", command))); err != nil {
		c.t.Fatal(err)
	}
	effect()
	if err := postToolUse(c.ctx, c.env(c.payload("PostToolUse", command))); err != nil {
		c.t.Fatal(err)
	}
}

// A file written after the call's commit is not that commit's: here a developer's staged
// change goes out first.
func TestCodexWriteAfterACommitDoesNotClaimIt(t *testing.T) {
	c := newCodexCall(t)
	hookruntest.WriteFile(t, c.root, "f.txt", "base\n")
	c.git("add", "f.txt")
	c.git("commit", "-qm", "base")
	hookruntest.WriteFile(t, c.root, "f.txt", "human\n")
	c.git("add", "f.txt")
	var human string
	c.run("git commit -m human && printf agent > f.txt", func() {
		human = c.commit("human")
		hookruntest.WriteFile(t, c.root, "f.txt", "agent")
	})
	if strings.Contains(human, "Agent-Session-Id") {
		t.Fatalf("the developer's commit was stamped by a write that came after it:\n%s", human)
	}
	c.git("add", "f.txt")
	if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+codexCallSession) {
		t.Fatalf("the agent's write, committed next, not stamped:\n%s", msg)
	}
}

// A write between two commits in one call claims neither: the first commit is the
// developer's staged change, made before the agent wrote anything.
func TestCodexWriteBetweenTwoCommitsDoesNotClaimTheFirst(t *testing.T) {
	c := newCodexCall(t)
	hookruntest.WriteFile(t, c.root, "h", "base\n")
	c.git("add", "h")
	c.git("commit", "-qm", "base")
	hookruntest.WriteFile(t, c.root, "h", "human\n")
	c.git("add", "h")
	var human string
	c.run("git commit -m human && printf agent > h && git add h && git commit -m agent", func() {
		human = c.commit("human")
		hookruntest.WriteFile(t, c.root, "h", "agent")
		c.git("add", "h")
		c.commit("agent")
	})
	if strings.Contains(human, "Agent-Session-Id") {
		t.Fatalf("the developer's commit was stamped by a write that came after it:\n%s", human)
	}
}

// After a call commits what it wrote, its PostToolUse leaves those files retired: a
// developer's later commit of them is not the session's.
func TestCodexSameCallCommitLeavesItsFilesRetired(t *testing.T) {
	for _, tc := range []struct {
		name, command, file string
		effect              func(c *codexCall)
	}{
		{"git mv", "git mv old.txt new.txt && git commit -m move", "new.txt", func(c *codexCall) {
			c.git("mv", "old.txt", "new.txt")
		}},
		{"wrapped commit", "printf x > w.txt && git add w.txt && command git commit -m w", "w.txt", func(c *codexCall) {
			hookruntest.WriteFile(c.t, c.root, "w.txt", "x")
			c.git("add", "w.txt")
		}},
		{"write", "printf '%s\\n' 'Terma Sandbox Team' > CONTRIBUTORS\n git add -- CONTRIBUTORS\n git commit -m \"Add CONTRIBUTORS with Terma Sandbox Team\"", "CONTRIBUTORS", func(c *codexCall) {
			hookruntest.WriteFile(c.t, c.root, "CONTRIBUTORS", "Terma Sandbox Team\n")
			c.git("add", "--", "CONTRIBUTORS")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodexCall(t)
			hookruntest.WriteFile(t, c.root, "old.txt", "old\n")
			c.git("add", "old.txt")
			c.git("commit", "-qm", "base")
			c.run(tc.command, func() {
				tc.effect(c)
				if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+codexCallSession) {
					t.Fatalf("commit in the call not stamped:\n%s", msg)
				}
			})
			if touched := hookruntest.Named(hookruntest.Spooled(t, c.sp), semconv.TermaFilesTouchedEvent); len(touched) != 1 {
				t.Fatalf("the call's files should be reported once, got %d events", len(touched))
			}
			hookruntest.WriteFile(t, c.root, tc.file, "human\n")
			c.git("add", tc.file)
			if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
				t.Fatalf("the developer's later commit of %s was stamped:\n%s", tc.file, msg)
			}
		})
	}
}

// endTurn runs the turn's Stop, with an empty CODEX_HOME.
func (c *codexCall) endTurn() {
	c.t.Helper()
	c.t.Setenv("CODEX_HOME", c.t.TempDir())
	if err := stop(c.ctx, c.env(c.payload("Stop", ""))); err != nil {
		c.t.Fatal(err)
	}
}

// A call the developer declines never runs and gets no PostToolUse: by the end of the turn
// its PreToolUse has claimed nothing, so the developer's own commit of the file is theirs.
// Reproduced live with Codex 0.160.1's app-server (TestCodexDeclinedWriteClaimsNothing).
func TestCodexDeclinedCallClaimsNothing(t *testing.T) {
	for _, command := range []string{
		"printf codex > NOTES.md",
		"printf codex > NOTES.md && git add NOTES.md && git commit -m notes",
	} {
		t.Run(command, func(t *testing.T) {
			c := newCodexCall(t)
			if err := preToolUse(c.ctx, c.env(c.payload("PreToolUse", command))); err != nil {
				t.Fatal(err)
			}
			c.endTurn()
			hookruntest.WriteFile(t, c.root, "NOTES.md", "mine\n")
			c.git("add", "NOTES.md")
			if msg := c.commit("my notes"); strings.Contains(msg, "Agent-Session-Id") {
				t.Fatalf("the developer's commit was stamped by a declined call:\n%s", msg)
			}
		})
	}
}

// The end of the turn withdraws only what a call claimed and left unchanged: a patch the
// shell ran, which gets no PostToolUse, keeps its claim, as does an earlier edit's.
func TestCodexEndOfTurnKeepsWhatCallsWrote(t *testing.T) {
	c := newCodexCall(t)
	hookruntest.WriteFile(t, c.root, "edited.txt", "base\n")
	c.git("add", "edited.txt")
	c.git("commit", "-qm", "base")
	c.run("printf agent > edited.txt", func() { hookruntest.WriteFile(t, c.root, "edited.txt", "agent") })
	if err := preToolUse(c.ctx, c.env(c.payload("PreToolUse", "printf again > edited.txt"))); err != nil {
		t.Fatal(err) // declined
	}
	patch := "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: patched.txt\n+patched\n*** End Patch\nPATCH"
	if err := preToolUse(c.ctx, c.env(c.payload("PreToolUse", patch))); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, c.root, "patched.txt", "patched\n")
	c.endTurn()
	c.git("add", "edited.txt", "patched.txt")
	if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+codexCallSession) {
		t.Fatalf("the session's edits lost their claim at the end of the turn:\n%s", msg)
	}
	for _, f := range []string{"edited.txt", "patched.txt"} {
		hookruntest.WriteFile(t, c.root, f, "human\n")
	}
	c.git("add", "edited.txt", "patched.txt")
	if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
		t.Fatalf("the developer's later commit was stamped:\n%s", msg)
	}
}

// Two parallel calls claim one file before either records it: one writes it, the other is
// declined. The declined call's unchanged record must not withdraw the other's claim.
func TestCodexParallelCallsKeepAWrittenClaim(t *testing.T) {
	c := newCodexCall(t)
	env := c.env("")
	r, err := env.Repo(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.Session{ID: codexCallSession, Tool: codexTool}
	files := env.Expect(r, sess, []string{filepath.Join(c.root, "f.txt")})
	expect(env, r, sess, nil, files) // the call that writes, before it runs
	hookruntest.WriteFile(t, c.root, "f.txt", "agent")
	expect(env, r, sess, nil, files) // the declined call, recorded late
	c.endTurn()
	c.git("add", "f.txt")
	if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+codexCallSession) {
		t.Fatalf("the declined call withdrew the parallel call's written claim:\n%s", msg)
	}
}
