package hookrun

import (
	"context"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
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
	// Each session's claim names its own working tree, the linked worktree's and not its main
	// checkout's, with git's name for the worktree and the branch its own HEAD is on; both share
	// the repository's origin.
	mainRoot, _ := filepath.EvalSymlinks(main)
	_, remote := gitx.OriginFS(filepath.Join(main, ".git"))
	for sid, want := range map[string]claim.Claim{
		"sess-wt":   {Root: wt, Worktree: "feature-x", Branch: "feature-x", Remote: remote},
		"sess-main": {Root: mainRoot, Branch: "main", Remote: remote},
	} {
		c, ok := claim.Read(stateDir, sid, time.Now())
		if !ok || c.Root != want.Root || c.Worktree != want.Worktree || c.Branch != want.Branch || c.Remote != want.Remote || c.Remote == "" {
			t.Errorf("%s claim = %+v, %v; want root %q, worktree %q, branch %q, remote %q", sid, c, ok, want.Root, want.Worktree, want.Branch, want.Remote)
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

// A session started in the main checkout that edits a file in a linked worktree of the same
// repository records it in the worktree's manifest, so a commit there carries its trailer;
// the edit does not claim the session for the worktree.
func TestAWorktreeCommitIsStampedForASessionInTheMainCheckout(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, ".claude", "worktrees", "feature-y")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	pol := hookruntest.Admitting(main)
	env := func(cwd, stdin string, args ...string) Env {
		return Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Policy: pol, Args: args, Stdin: strings.NewReader(stdin), Version: "test", Team: "proj-main"}
	}
	if err := startSession(ctx, env(main, `{"session_id":"sess-main","cwd":"`+hookruntest.InJSON(main)+`","hook_event_name":"SessionStart","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(wt, "src", "y.go")
	hookruntest.WriteFile(t, wt, "src/y.go", "package src\n")
	if err := editFile(ctx, env(main, `{"session_id":"sess-main","cwd":"`+hookruntest.InJSON(main)+`","tool_name":"Write","tool_input":{"file_path":"`+hookruntest.InJSON(file)+`"}}`)); err != nil {
		t.Fatal(err)
	}
	mainRoot, _ := filepath.EvalSymlinks(main)
	if c, ok := claim.Read(stateDir, "sess-main", time.Now()); !ok || c.Root != mainRoot {
		t.Errorf("after the edit, claim = %+v, %v; want the main checkout's", c.Root, ok)
	}
	if _, err := gitx.Git(ctx, wt, "add", "src/y.go"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	if err := os.WriteFile(msgPath, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommitMsg(ctx, env(wt, "", msgPath)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "sess-main" {
		t.Fatalf("the worktree commit's trailers = %+v, want sess-main's\n%s", got, data)
	}
}

// A session whose cwd is reported symlink-resolved, editing a file named through the link,
// records it in its own manifest: both name one checkout.
func TestAFileNamedThroughASymlinkIsTheSessionsOwn(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repo := hookruntest.InitRepo(t)
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(repo)
	sp, _ := spool.Open(t.TempDir())
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: resolvedRoot, Policy: hookruntest.Admitting(repo), Spool: sp, Version: "test", Team: "proj"}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, repo, "notes/n.md", "n\n")
	env.Touch(r, session.Session{ID: "sess-link", Tool: "claude-code"}, "Write", []string{filepath.Join(link, "notes", "n.md")}, nil)
	if got := manifestFiles(t, resolvedRoot, "sess-link"); got != "notes/n.md" {
		t.Errorf("manifest = %q, want notes/n.md", got)
	}
}

// A worktree's manifests mirrored from a main-checkout session expire like its own: a touch older than
// ManifestRetention, never committed, does not stamp a developer's commit of the file weeks later.
func TestAMirroredWorktreeManifestExpires(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, ".claude", "worktrees", "feature-z")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	pol := hookruntest.Admitting(main)
	now := time.Now()
	env := func(at time.Time, cwd, stdin string, args ...string) Env {
		return Env{StateDir: stateDir, Now: at, Cwd: cwd, Policy: pol, Args: args, Stdin: strings.NewReader(stdin), Version: "test", Team: "proj-main"}
	}
	edit := func(at time.Time, sess, rel string) {
		t.Helper()
		hookruntest.WriteFile(t, wt, rel, "package src\n")
		in := `{"session_id":"` + sess + `","cwd":"` + hookruntest.InJSON(main) + `"`
		if err := startSession(ctx, env(at, main, in+`,"hook_event_name":"SessionStart","source":"startup"}`)); err != nil {
			t.Fatal(err)
		}
		if err := editFile(ctx, env(at, main, in+`,"tool_name":"Write","tool_input":{"file_path":"`+hookruntest.InJSON(filepath.Join(wt, rel))+`"}}`)); err != nil {
			t.Fatal(err)
		}
	}
	edit(now.Add(-ManifestRetention-time.Hour), "sess-old", "src/y.go")
	edit(now, "sess-new", "src/z.go")

	if _, err := gitx.Git(ctx, wt, "add", "src/y.go", "src/z.go"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	if err := os.WriteFile(msgPath, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommitMsg(ctx, env(now, wt, "", msgPath)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "sess-new" {
		t.Fatalf("the worktree commit's trailers = %+v, want only sess-new's\n%s", got, data)
	}
}

// A mirrored worktree manifest older than ManifestRetention stamps nothing, though no later touch in the worktree
// pruned it: a developer's commit of the file weeks later is the developer's.
func TestAnExpiredMirroredManifestStampsNothing(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, ".claude", "worktrees", "feature-w")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	pol := hookruntest.Admitting(main)
	now := time.Now()
	old := now.Add(-ManifestRetention - time.Hour)
	env := func(at time.Time, cwd, stdin string, args ...string) Env {
		return Env{StateDir: stateDir, Now: at, Cwd: cwd, Policy: pol, Args: args, Stdin: strings.NewReader(stdin), Version: "test", Team: "proj-main"}
	}
	hookruntest.WriteFile(t, wt, "src/w.go", "package src\n")
	in := `{"session_id":"sess-old","cwd":"` + hookruntest.InJSON(main) + `"`
	if err := startSession(ctx, env(old, main, in+`,"hook_event_name":"SessionStart","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	if err := editFile(ctx, env(old, main, in+`,"tool_name":"Write","tool_input":{"file_path":"`+hookruntest.InJSON(filepath.Join(wt, "src/w.go"))+`"}}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, wt, "add", "src/w.go"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	if err := os.WriteFile(msgPath, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommitMsg(ctx, env(now, wt, "", msgPath)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 0 {
		t.Fatalf("the developer's commit carries %+v, want no trailer\n%s", got, data)
	}
}

// A hook's prediction before a tool call (Codex's PreToolUse) stays in the session's own checkout: a prediction
// mirrored into a linked worktree could not be withdrawn when the call is declined or writes nothing, and would stamp
// a developer's later commit there.
func TestAPredictionIsNotMirroredIntoAWorktree(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, ".claude", "worktrees", "feature-p")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	env := func(cwd string, args ...string) Env {
		return Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Policy: hookruntest.Admitting(main), Args: args, Version: "test", Team: "proj-main"}
	}
	r, err := env(main).resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, wt, "src/p.go", "package src\n")
	env(main).Expect(r, session.Session{ID: "sess-codex", Tool: "codex"}, []string{filepath.Join(wt, "src/p.go")})

	if _, err := gitx.Git(ctx, wt, "add", "src/p.go"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	if err := os.WriteFile(msgPath, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommitMsg(ctx, env(wt, msgPath)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 0 {
		t.Fatalf("the developer's commit carries %+v, want no trailer\n%s", got, data)
	}
}
