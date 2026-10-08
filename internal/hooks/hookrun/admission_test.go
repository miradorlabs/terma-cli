package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// namedRepo is a git repository in a folder called name, with remote origin when given.
func namedRepo(t *testing.T, name, remote string) string {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(parent, name)
	// Its own hooks path keeps a developer's global one out, as hookruntest.InitRepo's does.
	args := [][]string{{"init", "-q", "-b", "main", root}, {"-C", root, "config", "core.hooksPath", t.TempDir()},
		{"-C", root, "config", "user.email", "dev@example.com"}, {"-C", root, "config", "user.name", "Dev"}}
	if remote != "" {
		args = append(args, []string{"-C", root, "remote", "add", "origin", remote})
	}
	for _, a := range args {
		if _, err := gitx.Git(context.Background(), parent, a...); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func listing(entries ...string) config.Policy {
	p := config.DefaultPolicy()
	p.Repositories = entries
	return p
}

// fetched is p as a team's validated policy.
func fetched(p config.Policy) config.Policy {
	p.TeamID, p.FetchedAt = "t1", time.Now()
	return p
}

// markOf is sessionID's claim file when it holds only a mark: no project, no repository.
func markOf(t *testing.T, stateDir, sessionID string) (claim.Claim, string) {
	t.Helper()
	c, ok := claim.Read(stateDir, sessionID, time.Now())
	if !ok {
		t.Fatalf("%s was not marked", sessionID)
	}
	for _, p := range c.Placements {
		if p.ProjectID != "" || p.Repository != (config.Repository{}) || p.Repo != "" {
			t.Fatalf("%s is claimed, not marked: %+v", sessionID, c)
		}
	}
	dir := claim.Dir(stateDir)
	data, err := os.ReadFile(filepath.Join(dir, "claims", sessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return c, string(data)
}

// A checkout is admitted by origin alone, in any of git's URL forms, whatever its folder.
func TestAdmissionByOrigin(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	pol := listing("github.com/miradorlabs/mirador-platform")
	for _, tc := range []struct {
		folder, remote string
		want           bool
	}{
		{"mirador-platform", "git@github.com:miradorlabs/mirador-platform.git", true},
		{"checkout-a", "https://github.com/MiradorLabs/Mirador-Platform", true},
		{"checkout-b", "ssh://git@GitHub.com:22/miradorlabs/mirador-platform.git/", true},
		{"mirador-platform", "git@github.com:someone/mirador-platform.git", false},
		{"mirador-platform", "git@gitlab.com:miradorlabs/mirador-platform.git", false},
		{"mirador-platform", "", false},
		{"mirador-platform", "/srv/git/miradorlabs/mirador-platform.git", false},
	} {
		root := namedRepo(t, tc.folder, tc.remote)
		_, err := Env{StateDir: stateDir, Cwd: root, Policy: pol, Team: "t1"}.Repo(t.Context())
		if got := err == nil; got != tc.want {
			t.Errorf("%s (%s): admitted %v, err %v", tc.folder, tc.remote, got, err)
		}
	}
}

// Outside git nothing is admitted, whatever the folder is called.
func TestAdmissionOutsideGit(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(parent, "mirador-platform")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (Env{StateDir: stateDir, Cwd: dir, Policy: listing("github.com/miradorlabs/mirador-platform"), Team: "t1"}).Repo(t.Context()); err == nil {
		t.Fatal("a folder outside git was admitted")
	}
}

// A hook in a repository the list does not name records nothing: no spool line, no claim,
// no session state, no trailer. It only marks the session not collected, naming neither
// the team nor the repository, so the relay drops its telemetry at once.
func TestAHookOutsideTheListOnlyMarksTheSession(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	root := namedRepo(t, "personal", "git@github.com:me/personal.git")
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) Env {
		return Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Team: "t1", Policy: fetched(listing("github.com/acme/work"))}
	}
	ctx := t.Context()
	if err := startSession(ctx, env(`{"session_id":"s1","cwd":"`+hookruntest.InJSON(root)+`"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "a.txt", "a\n")
	if err := editFile(ctx, env(`{"session_id":"s1","cwd":"`+hookruntest.InJSON(root)+`","tool_name":"Edit","tool_input":{"file_path":"`+hookruntest.InJSON(filepath.Join(root, "a.txt"))+`"}}`)); err != nil {
		t.Fatal(err)
	}
	ClaimFromPayload(ctx, env(""), PayloadSession{ID: "s1", Cwd: root}, "claude-code")
	if _, err := gitx.Git(ctx, root, "add", "a.txt"); err != nil {
		t.Fatal(err)
	}
	msg := filepath.Join(t.TempDir(), "MSG")
	hookruntest.WriteFile(t, filepath.Dir(msg), "MSG", "work\n")
	if err := PrepareCommitMsg(ctx, env("", msg, "message")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msg); err != nil {
		t.Fatal(err)
	}
	if err := PostCommit(ctx, env("")); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.ReadFile(t, filepath.Dir(msg), "MSG"); got != "work\n" {
		t.Fatalf("the commit message was stamped: %q", got)
	}
	if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
		t.Fatalf("spooled %s", hookruntest.Names(evs))
	}
	c, raw := markOf(t, stateDir, "s1")
	if c.Tool != "claude-code" || len(c.PIDs) == 0 {
		t.Errorf("the mark names no agent or process: %+v", c)
	}
	for _, leak := range []string{"personal", "t1", "github.com", filepath.ToSlash(root)} {
		if strings.Contains(raw, leak) {
			t.Errorf("the mark holds %q: %s", leak, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "terma")); !os.IsNotExist(err) {
		t.Fatalf("session state was written: %v", err)
	}
}

// A session started in a subdirectory, or in a linked worktree whose .git is a file, is
// its checkout's, admitted by the origin the worktree reads from its main repository.
func TestASubdirectorySessionIsItsCheckouts(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := namedRepo(t, "checkout-a", "git@github.com:miradorlabs/mirador-platform.git")
	ctx := t.Context()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(filepath.Dir(main), "feature")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	for i, cwd := range []string{filepath.Join(main, "src", "pkg"), filepath.Join(wt, "docs")} {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		sid := []string{"from-subdir", "from-worktree"}[i]
		env := Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Team: "t1", Policy: listing("github.com/miradorlabs/mirador-platform"),
			Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + hookruntest.InJSON(cwd) + `"}`)}
		if err := startSession(ctx, env); err != nil {
			t.Fatal(err)
		}
		c, ok := claim.Read(stateDir, sid, time.Now())
		if !ok || c.Repository.Origin != "github.com/miradorlabs/mirador-platform" || c.Repo != "checkout-a" {
			t.Errorf("%s: claim %+v, %v", sid, c, ok)
		}
	}
}

// A session whose next turn runs in a repository the list does not name is marked there:
// its claim's latest placement names no project and no repository, which the relay drops,
// while the earlier one stays.
func TestATurnInAnUnlistedRepositoryMarksTheClaim(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	work := namedRepo(t, "work", "git@github.com:acme/work.git")
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	pol := listing("github.com/acme/work")
	ctx := t.Context()
	at := time.Now()
	if !ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: at, Cwd: work, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: work}, "codex") {
		t.Fatal("the admitted turn was not claimed")
	}
	if ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: at.Add(time.Minute), Cwd: personal, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: personal}, "codex") {
		t.Fatal("the unlisted turn claimed")
	}
	c, ok := claim.Read(stateDir, "thread", at.Add(time.Minute))
	if !ok || len(c.Placements) != 2 {
		t.Fatalf("claim %+v, %v", c, ok)
	}
	if last := c.Placements[1]; last.ProjectID != "" || last.Repository != (config.Repository{}) || !pol.Admits(c.Placements[0].Repository) {
		t.Fatalf("placements %+v: want a mark latest, the admitted one kept", c.Placements)
	}
}

// A session marked in an unlisted repository is claimed once a turn runs in a listed one:
// the mark stays first, so what it sent before stays dropped.
func TestAMarkedSessionIsClaimedInAListedRepository(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	work := namedRepo(t, "work", "git@github.com:acme/work.git")
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	pol := fetched(listing("github.com/acme/work"))
	ctx := t.Context()
	at := time.Now()
	if ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: at, Cwd: personal, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: personal}, "codex") {
		t.Fatal("the unlisted turn reported a claim")
	}
	markOf(t, stateDir, "thread")
	if !ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: at.Add(time.Minute), Cwd: work, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: work}, "codex") {
		t.Fatal("the listed turn was not claimed")
	}
	c, ok := claim.Read(stateDir, "thread", at.Add(time.Minute))
	if !ok || len(c.Placements) != 2 || c.Placements[0].ProjectID != "" || c.ProjectID != "t1" || !pol.Admits(c.Repository) {
		t.Fatalf("claim %+v, %v", c, ok)
	}
}

// Every working copy the validated policy does not admit is marked, outside git and a
// subagent's own id included; a policy not yet validated, or global mode, marks nothing.
func TestWhereAHookMarks(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	ctx := t.Context()
	listed := fetched(listing("github.com/acme/work"))
	mark := func(dir, sid, agent string, pol config.Policy) {
		ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: time.Now(), Cwd: dir, Team: "t1", Policy: pol}, PayloadSession{ID: sid, AgentID: agent, Cwd: dir}, "claude-code")
	}
	mark(outside, "outside", "", listed)
	markOf(t, stateDir, "outside")
	mark(personal, "parent", "child", listed)
	markOf(t, stateDir, "parent")
	markOf(t, stateDir, "child")
	mark(personal, "unvalidated", "", listing("github.com/acme/work"))
	if _, ok := claim.Read(stateDir, "unvalidated", time.Now()); ok {
		t.Error("a policy not yet validated marked a session")
	}
	global := fetched(config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"})
	mark(personal, "global", "", global)
	if c, ok := claim.Read(stateDir, "global", time.Now()); !ok || c.ProjectID != "p-default" || len(c.Placements) != 1 {
		t.Errorf("global mode: claim %+v, %v; want the default project's, no mark", c, ok)
	}
}

// A policy the relay has stopped refreshing may not list a repository the team now
// collects: a hook outside the list still marks its session and starts a refresh, at most
// once per config.PolicyStaleAfter per team, so the next hook under the refreshed policy
// claims the session.
func TestAStalePolicyStartsARefresh(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	ctx := t.Context()
	at := time.Now()
	stale := fetched(listing("github.com/acme/work"))
	stale.FetchedAt = at.Add(-10 * time.Minute)
	otherTeam := stale
	otherTeam.TeamID = "t2"
	flushes := 0
	hook := func(sid string, pol config.Policy, now time.Time) bool {
		return ClaimFromPayload(ctx, Env{StateDir: stateDir, Now: now, Cwd: personal, Team: "t1", Policy: pol, Flush: func() { flushes++ }}, PayloadSession{ID: sid, Cwd: personal}, "claude-code")
	}
	hook("s1", stale, at)
	markOf(t, stateDir, "s1")
	hook("s1", stale, at.Add(30*time.Second))
	if flushes != 1 {
		t.Fatalf("%d refreshes in half a minute, want 1", flushes)
	}
	hook("s2", otherTeam, at.Add(30*time.Second))
	if flushes != 2 {
		t.Fatalf("another team's refresh waited on this one's: %d refreshes", flushes)
	}
	hook("s1", stale, at.Add(55*time.Second))
	if flushes != 3 {
		t.Fatalf("%d refreshes after 55 seconds, want 3", flushes)
	}
	unlock, err := flock.TryLock(filepath.Join(stateDir, config.PoliciesDir, refreshedDir, "t1.lock"))
	if err != nil {
		t.Fatal(err)
	}
	hook("s1", stale, at.Add(5*time.Minute))
	unlock()
	if flushes != 3 {
		t.Fatal("a hook started a refresh while another held the stamp")
	}
	hook("fresh", fetched(listing("github.com/acme/work")), at)
	markOf(t, stateDir, "fresh")
	if flushes != 3 {
		t.Fatal("a fresh policy started a refresh")
	}
	if !hook("s1", fetched(listing("github.com/acme/work", "github.com/me/personal")), at.Add(3*time.Minute)) {
		t.Fatal("the refreshed listing did not claim the session")
	}
	if c, _ := claim.Read(stateDir, "s1", at.Add(3*time.Minute)); c.ProjectID != "t1" || len(c.Placements) != 2 {
		t.Fatalf("claim %+v: want the mark, then the listed placement", c)
	}
}

// BenchmarkClaimFromPayloadUnlisted is an agent hook in an unlisted repository once its
// session is marked: admission, then the mark's fast path.
func BenchmarkClaimFromPayloadUnlisted(b *testing.B) {
	stateDir := b.TempDir()
	parent, _ := filepath.EvalSymlinks(b.TempDir())
	root := filepath.Join(parent, "personal")
	for _, a := range [][]string{{"init", "-q", root}, {"-C", root, "remote", "add", "origin", "git@github.com:me/personal.git"}} {
		if _, err := gitx.Git(b.Context(), parent, a...); err != nil {
			b.Fatal(err)
		}
	}
	tok := claim.TokenPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(tok), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("local"), 0o600); err != nil {
		b.Fatal(err)
	}
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Team: "t1", Policy: fetched(listing("github.com/acme/work"))}
	s := PayloadSession{ID: "bench", Cwd: root}
	ClaimFromPayload(b.Context(), env, s, "claude-code")
	for b.Loop() {
		ClaimFromPayload(b.Context(), env, s, "claude-code")
	}
}

// Admits judges another directory as Repo judges the current one, wherever the hook runs.
func TestAdmitsJudgesTheDirectoryItIsGiven(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	listed := namedRepo(t, "listed", "git@github.com:acme/listed.git")
	other := namedRepo(t, "other", "git@github.com:acme/other.git")
	if err := os.MkdirAll(filepath.Join(listed, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := Env{StateDir: stateDir, Cwd: other, Policy: listing("github.com/acme/listed")}
	for dir, want := range map[string]bool{
		listed: true, filepath.Join(listed, "sub"): true, other: false, filepath.Join(t.TempDir(), "gone"): false,
	} {
		if got := e.Admits(context.Background(), dir); got != want {
			t.Errorf("Admits(%s) = %v, want %v", dir, got, want)
		}
	}
}
