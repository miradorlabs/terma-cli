package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A linked worktree's events go to the developer's team, as the main checkout's do.
func TestWorktreeEventsReportTheMainRepositoryAndProject(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "feature-x")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	wt, _ = filepath.EvalSymlinks(wt)

	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pol := hookruntest.Admitting(main)
	run := func(cwd, payload string, hook func(context.Context, Env) error) {
		t.Helper()
		if err := hook(ctx, Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Policy: pol, Stdin: strings.NewReader(payload), Spool: sp, Version: "test", Team: "proj-main"}); err != nil {
			t.Fatal(err)
		}
	}
	hookruntest.WriteFile(t, wt, "src/agent.go", "package src\n")
	run(wt, `{"session_id":"sess-wt","cwd":"`+hookruntest.InJSON(wt)+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)
	run(wt, `{"session_id":"sess-wt","cwd":"`+hookruntest.InJSON(wt)+`","tool_name":"Write","tool_input":{"file_path":"`+hookruntest.InJSON(filepath.Join(wt, "src", "agent.go"))+`"}}`, editFile)
	run(main, `{"session_id":"sess-main","cwd":"`+hookruntest.InJSON(main)+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)

	events := hookruntest.Spooled(t, sp)
	if len(events) < 3 {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events {
		if e.Attrs[AttrProjectID] != "proj-main" {
			t.Fatalf("%s from %s: attrs %v", e.Name, e.SessionID, e.Attrs)
		}
	}
	// Each session's claim names its own working tree, the linked worktree's and not its main checkout's.
	mainRoot, _ := filepath.EvalSymlinks(main)
	for sid, want := range map[string]string{"sess-wt": wt, "sess-main": mainRoot} {
		if c, ok := claim.Read(stateDir, sid, time.Now()); !ok || c.Root != want {
			t.Errorf("%s claim root = %q, %v; want %q", sid, c.Root, ok, want)
		}
	}
}

// A session that reached its checkout through a symlink still names the resolved working
// tree, on its claim and on its commit's record, as the platform compares paths.
func TestTheWorkingTreeIsSymlinkResolved(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repo := hookruntest.InitRepo(t)
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	sp, _ := spool.Open(t.TempDir())
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: link, Policy: hookruntest.Admitting(repo), Spool: sp, Version: "test", Team: "proj",
		Stdin: strings.NewReader(`{"session_id":"sess-link","cwd":"` + hookruntest.InJSON(link) + `","hook_event_name":"SessionStart"}`)}
	if err := startSession(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read(stateDir, "sess-link", time.Now()); !ok || c.Root != want || c.Cwd != want {
		t.Errorf("claim root, cwd = %q, %q, %v; want %q", c.Root, c.Cwd, ok, want)
	}
	if _, err := gitx.Git(context.Background(), repo, "commit", "-q", "--allow-empty", "-m", "by hand"); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader("")
	if err := PostCommit(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	commits := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaCommitUnattributedEvent)
	if len(commits) != 1 {
		t.Fatalf("want the commit's record, got %d", len(commits))
	}
	for _, e := range commits {
		if e.Attrs[semconv.TermaRepositoryRootKey] != want {
			t.Errorf("commit root = %v, want %q", e.Attrs[semconv.TermaRepositoryRootKey], want)
		}
	}
}
