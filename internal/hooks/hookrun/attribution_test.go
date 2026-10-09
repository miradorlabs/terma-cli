//go:build unix

package hookrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// Real sessions, read back from prd log_records: a Claude Code session and the Codex thread
// of the terma-cli#32 reproduction.
const (
	claudeSession = "5e757e6f-3040-4c57-b37d-01d44cc43053"
	codexSession  = "01a11fd5-7d48-7843-b64c-2cf93f9b39b5"
)

// commitAs stages rel and runs the commit hooks around git commit, as git does, returning HEAD.
func commitAs(t *testing.T, env func(args ...string) Env, root, msg string, rel ...string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := gitx.Git(ctx, root, append([]string{"add"}, rel...)...); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msgPath, []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareCommitMsg(ctx, env(msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}
	if err := PostCommit(ctx, env()); err != nil {
		t.Fatal(err)
	}
	sha, _ := gitx.Git(ctx, root, "rev-parse", "HEAD")
	return strings.TrimSpace(sha)
}

// terma-cli#32: Codex runs `git commit … && git log -1` in its workspace-write sandbox, where
// the commit's hooks may write .git but not the state directory. The commit's events wait in
// the repository's store, and the next hook outside the sandbox spools them, once.
func TestASandboxedCommitIsSpooledByTheNextHook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	root := hookruntest.InitRepo(t)
	spoolDir := t.TempDir()
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	flushes := 0
	env := func(args ...string) Env {
		return Env{StateDir: t.TempDir(), Now: now, Cwd: root, Policy: hookruntest.Admitting(root), Args: args,
			Spool: sp, Team: hookruntest.Team, Flush: func() { flushes++ }}
	}
	codex := session.Session{ID: codexSession, Tool: "codex", ToolVersion: "0.160.1"}
	if err := hookruntest.Store(t, root).Touch(codex, []string{"pairs/e4b.txt"}, now); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "pairs/e4b.txt", "b\n")

	// The sandbox: opening the spool's lock is refused, as Codex's seatbelt refused it.
	if err := os.Chmod(spoolDir, 0o500); err != nil {
		t.Fatal(err)
	}
	sha := commitAs(t, env, root, "e4 b then log", "pairs/e4b.txt")
	if entries, _ := os.ReadDir(spoolDir); len(entries) != 0 {
		t.Fatalf("the sandboxed hooks reached the spool: %v", entries)
	}
	if msg, _ := gitx.Git(context.Background(), root, "log", "-1", "--format=%B"); !strings.Contains(msg, trailer.KeySessionID+": "+codexSession) {
		t.Fatalf("the commit was not stamped:\n%s", msg)
	}

	// Codex's PostToolUse hook runs outside the sandbox, and resolves the repository first.
	if err := os.Chmod(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := env().Repo(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := hookruntest.Spooled(t, sp)
	if names := hookruntest.Names(got); names != semconv.TermaCommitStampedEvent+" "+semconv.TermaCommitEvent {
		t.Fatalf("spooled %q, want the stamp and the commit once each", names)
	}
	commit := got[1]
	if commit.Attrs[semconv.VCSRefHeadRevisionKey] != sha || commit.SessionID != codexSession || !commit.Time.Equal(now) ||
		commit.Attrs[AttrProjectID] != hookruntest.Team {
		t.Errorf("terma.commit = %+v, want %s at the commit's time", commit, sha)
	}
	if flushes != 1 {
		t.Errorf("%d flushes after unparking, want 1", flushes)
	}
}

// terma-cli#34: one commit carries a Claude Code session, a Codex session, and a Codex
// session whose id is the Claude Code session's. Each stays its own, with its own agent,
// from the manifests to the trailers, the file owners, the manifests retired and the export.
func TestAMixedCommitKeepsEachSessionsAgent(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	now := time.Now()
	env := func(args ...string) Env {
		return Env{StateDir: stateDir, Now: now, Cwd: root, Policy: hookruntest.Admitting(root), Args: args, Spool: sp, Team: hookruntest.Team}
	}
	store := hookruntest.Store(t, root)
	for _, touch := range []struct {
		sess session.Session
		file string
	}{
		{session.Session{ID: claudeSession, Tool: "claude-code", ToolVersion: "2.1.3"}, "a.txt"},
		{session.Session{ID: codexSession, Tool: "codex", ToolVersion: "0.160.1"}, "b.txt"},
		{session.Session{ID: claudeSession, Tool: "codex", ToolVersion: "0.160.1"}, "c.txt"},
	} {
		if err := store.Touch(touch.sess, []string{touch.file}, now); err != nil {
			t.Fatal(err)
		}
		hookruntest.WriteFile(t, root, touch.file, touch.file+"\n")
		now = now.Add(time.Second)
	}
	commitAs(t, env, root, "mixed", "a.txt", "b.txt", "c.txt")

	msg, _ := gitx.Git(context.Background(), root, "log", "-1", "--format=%B")
	want := []trailer.Trailer{
		{SessionID: claudeSession, Tool: "claude-code/2.1.3"},
		{SessionID: codexSession, Tool: "codex/0.160.1"},
		{SessionID: claudeSession, Tool: "codex/0.160.1"},
	}
	if got := trailer.Parse(msg, "#"); !reflect.DeepEqual(got, want) {
		t.Fatalf("trailers = %+v, want %+v", got, want)
	}

	commits := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaCommitEvent)
	if len(commits) != 1 {
		t.Fatalf("%d terma.commit events", len(commits))
	}
	a := commits[0].Attrs
	if got := sessionMaps(a[semconv.TermaCommitSessionsKey]); !reflect.DeepEqual(got, []map[string]any{
		{"session_id": claudeSession, "agent": "claude-code", "agent_version": "2.1.3"},
		{"session_id": codexSession, "agent": "codex", "agent_version": "0.160.1"},
		{"session_id": claudeSession, "agent": "codex", "agent_version": "0.160.1"},
	}) {
		t.Errorf("terma.commit.sessions = %v", got)
	}
	if _, ok := a[semconv.GenAIMainAgentNameKey]; ok || hookruntest.Num(a[semconv.TermaCommitSessionCountKey]) != 3 ||
		hookruntest.Joined(a[semconv.TermaCommitSessionIDsKey]) != claudeSession+","+codexSession {
		t.Errorf("a mixed commit: main agent %v, count %v, ids %v", a[semconv.GenAIMainAgentNameKey], a[semconv.TermaCommitSessionCountKey], a[semconv.TermaCommitSessionIDsKey])
	}
	owners := map[string][2]any{}
	for _, f := range sessionMaps(a[semconv.TermaCommitFileStatsKey]) {
		owners[f["path"].(string)] = [2]any{f["agent"], f["session_id"]}
	}
	if !reflect.DeepEqual(owners, map[string][2]any{
		"a.txt": {"claude-code", claudeSession}, "b.txt": {"codex", codexSession}, "c.txt": {"codex", claudeSession},
	}) {
		t.Errorf("file owners = %v", owners)
	}
	manifests, _ := store.Manifests()
	for _, m := range manifests {
		if len(m.Files) != 0 {
			t.Errorf("%s %s still holds %v", m.Tool, m.SessionID, m.Files)
		}
	}
	if len(manifests) != 3 {
		t.Errorf("%d manifests, want each session's kept", len(manifests))
	}
}

// A session stamped without Agent-Tool, which terma never writes, is exported as it is: no
// agent, rather than another session's or a stand-in.
func TestASessionWithNoAgentHasNoAgent(t *testing.T) {
	t.Parallel()
	got := sessionsAttr(uniqueSessions(trailer.Parse("x\n\nAgent-Session-Id: s1\nAgent-Session-Id: s2\nAgent-Tool: codex\n", "#")))
	if !reflect.DeepEqual(got, []map[string]any{{"session_id": "s1"}, {"session_id": "s2", "agent": "codex"}}) {
		t.Errorf("sessions = %v", got)
	}
	if agent, ok := soleAgent([]trailer.Trailer{{SessionID: "s1"}}); ok {
		t.Errorf("no agent was named, yet the main agent is %q", agent)
	}
}

// A push from the sandbox: pre-push cannot record it in the state directory, so the record
// waits in the repository's store, and the next hook outside the sandbox reports it, once.
// The commit's trailers are a real mixed commit's (3e6c16f in terma-sim-sandbox).
func TestASandboxedPushIsReportedByTheNextHook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	// git push has exited by the time a hook outside the sandbox runs.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return gone.Process.Pid }
	p := newPushRepo(t)
	p.git("checkout", "-q", "-b", "feature")
	p.git("commit", "-q", "--allow-empty", "-m", "pairs: mixed Claude Code and Codex commit\n\n"+
		"Agent-Session-Id: 01a11feb-88f3-7f92-9137-144b0ac92b7e\nAgent-Tool: codex\n"+
		"Agent-Session-Id: 0102cabd-6dca-42ce-9888-c0e010ece99f\nAgent-Tool: claude-code\n")
	tip := p.sha("HEAD")
	ctx := context.Background()
	awaited := 0
	var env func(stdin string) Env
	env = func(stdin string) Env {
		return Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"backup", p.remote},
			Stdin: strings.NewReader(stdin), Spool: p.sp, Team: p.team, Policy: hookruntest.Admitting(p.root),
			AwaitPush: func(path string) { awaited++; AwaitPush(ctx, env(""), path) }}
	}

	pushes := filepath.Join(p.stateDir, PushesDir)
	if err := os.MkdirAll(pushes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pushes, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := PrePush(ctx, env(pushLine("refs/heads/feature", tip, "refs/heads/feature", zero)+"\n")); err != nil {
		t.Fatal(err)
	}
	p.git("push", "-q", "backup", "feature")
	if awaited != 0 {
		t.Fatal("a push the state directory refused was awaited")
	}

	if err := os.Chmod(pushes, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := env("").Repo(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := hookruntest.Named(hookruntest.Spooled(t, p.sp), semconv.TermaPushEvent)
	if awaited != 1 || len(got) != 1 {
		t.Fatalf("awaited %d, reported %d pushes, want 1 each", awaited, len(got))
	}
	a := got[0].Attrs
	if a[semconv.TermaPushNewRevisionKey] != tip || a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusTrackingRefUpdated ||
		!reflect.DeepEqual(sessionMaps(a[semconv.TermaPushSessionsKey]), []map[string]any{
			{"session_id": "01a11feb-88f3-7f92-9137-144b0ac92b7e", "agent": "codex"},
			{"session_id": "0102cabd-6dca-42ce-9888-c0e010ece99f", "agent": "claude-code"},
		}) {
		t.Errorf("terma.push = %+v", a)
	}
}
