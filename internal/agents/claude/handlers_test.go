package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

func TestClaudeSessionStampsOnlyItsOwnFiles(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: now, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
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
