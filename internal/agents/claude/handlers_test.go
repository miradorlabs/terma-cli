package claude

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
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

func TestClaudeSessionStampsOnlyItsOwnFiles(t *testing.T) {
	stateDir := t.TempDir()
	root := newRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Now: now, Cwd: root, Policy: hookruntest.Admitting(root), Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}

	if err := sessionStart(ctx, env(`{"session_id":"sess-claude-1","cwd":"`+root+`","hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/agent.go", "package src\n")
	hookruntest.WriteFile(t, root, "notes/human.md", "mine\n")
	if err := postToolUse(ctx, env(`{"session_id":"sess-claude-1","cwd":"`+root+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(root, "src", "agent.go")+`"}}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, root, "add", "notes/human.md"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("human note\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("human-only commit must not be stamped:\n%s", data)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, root, "add", "src/agent.go"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(msgPath, []byte("Add agent code\n\n# comment\n"), 0o644)
	start := time.Now()
	if err := hookrun.PrepareCommitMsg(ctx, env("", msgPath, "")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Logf("warning: prepare-commit-msg took %s", elapsed)
	}
	data, _ := os.ReadFile(msgPath)
	got := trailer.Parse(string(data), "#")
	if len(got) != 1 || got[0].SessionID != "sess-claude-1" || got[0].Tool != "claude-code" {
		t.Fatalf("unexpected trailers %+v in:\n%s", got, data)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}
	if err := hookrun.PostCommit(ctx, env("")); err != nil {
		t.Fatal(err)
	}

	// The committed file is consumed: a later unrelated commit is clean though the session is active.
	hookruntest.WriteFile(t, root, "notes/again.md", "more\n")
	_, _ = gitx.Git(ctx, root, "add", "notes/again.md")
	_ = os.WriteFile(msgPath, []byte("more notes\n"), 0o644)
	_ = hookrun.PrepareCommitMsg(ctx, env("", msgPath, ""))
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("work already committed must not re-stamp a later human commit, even with the session still active:\n%s", data)
	}

	n, _, _ := sp.Pending()
	if n != 5 {
		t.Fatalf("expected 5 spooled events, got %d", n)
	}
	var names []string
	res := sp.Flush(ctx, spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		for _, e := range events {
			names = append(names, e.Name)
		}
		return nil, nil
	}), spool.FlushOptions{})
	if res.Err != nil || strings.Join(names, " ") != "terma.session.start terma.session.account terma.files.touched terma.commit.stamped terma.commit" {
		t.Fatalf("unexpected events %v (%v)", names, res.Err)
	}
}

// claudeBash runs a Bash call's hooks around git: PreToolUse, then the call's own effect (its
// commits made through commit), then PostToolUse.
type claudeBash struct {
	t        *testing.T
	ctx      context.Context
	root     string
	stateDir string
	sp       *spool.Spool
}

const claudeBashSession = "5b0e9a52-6c11-4a3e-9d3f-2f1c0a7b8e41"

func newClaudeBash(t *testing.T) *claudeBash {
	sp, _ := spool.Open(t.TempDir())
	c := &claudeBash{t: t, ctx: context.Background(), root: hookruntest.InitRepo(t), stateDir: t.TempDir(), sp: sp}
	hookruntest.WriteFile(t, c.root, "base.txt", "base\n")
	c.git("add", "base.txt")
	c.git("commit", "-qm", "base")
	return c
}

func (c *claudeBash) env(stdin string, args ...string) hookrun.Env {
	return hookrun.Env{StateDir: c.stateDir, Now: time.Now(), Cwd: c.root, Policy: hookruntest.Admitting(c.root), Args: args, Stdin: strings.NewReader(stdin), Spool: c.sp, Version: "test"}
}

func (c *claudeBash) payload(event, command string) string {
	return `{"session_id":"` + claudeBashSession + `","transcript_path":"","cwd":` + strconv.Quote(c.root) + `,"hook_event_name":"` + event +
		`","model":"claude-opus-5-5","tool_name":"Bash","tool_input":{"command":` + strconv.Quote(command) + `},"tool_use_id":"toolu_01","tool_response":{}}`
}

func (c *claudeBash) git(args ...string) {
	c.t.Helper()
	if _, err := gitx.Git(c.ctx, c.root, args...); err != nil {
		c.t.Fatal(err)
	}
}

// commit commits what is staged as the commit hooks see it, and returns its message.
func (c *claudeBash) commit(message string) string {
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

// call runs a Bash call; a nil effect is a call the developer declined, which never runs and
// gets no PostToolUse.
func (c *claudeBash) call(command string, effect func()) {
	c.t.Helper()
	if err := preToolUse(c.ctx, c.env(c.payload("PreToolUse", command))); err != nil {
		c.t.Fatal(err)
	}
	if effect == nil {
		return
	}
	effect()
	if err := postToolUse(c.ctx, c.env(c.payload("PostToolUse", command))); err != nil {
		c.t.Fatal(err)
	}
}

func (c *claudeBash) endTurn() {
	c.t.Helper()
	if err := stop(c.ctx, c.env(`{"session_id":"`+claudeBashSession+`","transcript_path":"","cwd":`+strconv.Quote(c.root)+`,"hook_event_name":"Stop"}`)); err != nil {
		c.t.Fatal(err)
	}
}

// A Bash call that writes a file and commits it in one command stamps that commit, and leaves
// the file retired: the developer's later commit of it is not the session's.
func TestClaudeBashCommitInTheWritingCallIsStamped(t *testing.T) {
	c := newClaudeBash(t)
	c.call("printf agent > out.txt && git add out.txt && git commit -m agent", func() {
		hookruntest.WriteFile(t, c.root, "out.txt", "agent")
		c.git("add", "out.txt")
		if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+claudeBashSession) {
			t.Fatalf("commit in the writing call not stamped:\n%s", msg)
		}
	})
	if touched := hookruntest.Named(hookruntest.Spooled(t, c.sp), semconv.TermaFilesTouchedEvent); len(touched) != 1 {
		t.Fatalf("the call's files should be reported once, got %d events", len(touched))
	}
	c.endTurn()
	hookruntest.WriteFile(t, c.root, "out.txt", "human\n")
	c.git("add", "out.txt")
	if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
		t.Fatalf("the developer's later commit was stamped:\n%s", msg)
	}
}

// A file a Bash call writes (sed -i, a redirect) and a later call commits is the session's.
func TestClaudeBashWriteCommittedLaterIsStamped(t *testing.T) {
	c := newClaudeBash(t)
	c.call("sed -i '' 's/base/agent/' base.txt", func() { hookruntest.WriteFile(t, c.root, "base.txt", "agent\n") })
	c.endTurn()
	c.git("add", "base.txt")
	if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+claudeBashSession) {
		t.Fatalf("a Bash write committed later not stamped:\n%s", msg)
	}
}

// A file a call names after its commit, only on a condition, is a guess: left unchanged, the
// turn's Stop withdraws it, so the developer's later commit of it is not the session's.
func TestClaudeBashGuessAfterACommitIsWithdrawn(t *testing.T) {
	c := newClaudeBash(t)
	hookruntest.WriteFile(t, c.root, "notes.txt", "base\n")
	c.git("add", "notes.txt")
	c.git("commit", "-qm", "notes")
	c.call("git commit --allow-empty -m agent; test -e sentinel && echo x > notes.txt; true", func() {
		c.git("commit", "-q", "--allow-empty", "-m", "agent")
	})
	c.endTurn()
	hookruntest.WriteFile(t, c.root, "notes.txt", "human\n")
	c.git("add", "notes.txt")
	if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
		t.Fatalf("an unwritten guess after the call's commit stamped the developer's commit:\n%s", msg)
	}
}

// A path a call writes, commits, then names again on a condition it never meets is the
// committed write's alone: the developer's later commit of it is not the session's.
func TestClaudeBashRepeatedGuessAfterACommitIsNotClaimed(t *testing.T) {
	c := newClaudeBash(t)
	c.call("printf agent > f; git add f; git commit -m agent; test -e sentinel && printf later > f", func() {
		hookruntest.WriteFile(t, c.root, "f", "agent")
		c.git("add", "f")
		if msg := c.commit("agent"); !strings.Contains(msg, "Agent-Session-Id: "+claudeBashSession) {
			t.Fatalf("commit in the writing call not stamped:\n%s", msg)
		}
	})
	c.endTurn()
	hookruntest.WriteFile(t, c.root, "f", "human\n")
	c.git("add", "f")
	if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
		t.Fatalf("an unmet guess at a committed path stamped the developer's commit:\n%s", msg)
	}
}

// A Bash call the developer declines claims nothing once the turn is over: declining
// interrupts the turn, which then has no Stop, so the next prompt, a failed turn's
// StopFailure or the session's end settles it.
func TestClaudeDeclinedBashCallClaimsNothing(t *testing.T) {
	for _, tc := range []struct {
		event string
		hook  func(context.Context, hookrun.Env) error
	}{
		{"UserPromptSubmit", userPromptSubmit},
		{"StopFailure", stopFailure},
		{"SessionEnd", sessionEnd},
	} {
		t.Run(tc.event, func(t *testing.T) {
			c := newClaudeBash(t)
			c.call("printf agent > base.txt", nil)
			payload := `{"session_id":"` + claudeBashSession + `","transcript_path":"","cwd":` + strconv.Quote(c.root) + `,"hook_event_name":"` + tc.event + `"}`
			if err := tc.hook(c.ctx, c.env(payload)); err != nil {
				t.Fatal(err)
			}
			hookruntest.WriteFile(t, c.root, "base.txt", "human\n")
			c.git("add", "base.txt")
			if msg := c.commit("human"); strings.Contains(msg, "Agent-Session-Id") {
				t.Fatalf("a declined call's claim stamped the developer's commit after %s:\n%s", tc.event, msg)
			}
		})
	}
}
