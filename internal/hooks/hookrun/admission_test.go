package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// namedRepo is a git repository in a folder called name, with remote origin when given,
// in the sandbox initRepo set up.
func namedRepo(t *testing.T, name, remote string) string {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(parent, name)
	args := [][]string{{"init", "-q", "-b", "main", root}, {"-C", root, "config", "user.email", "dev@example.com"}, {"-C", root, "config", "user.name", "Dev"}}
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
func markOf(t *testing.T, sessionID string) (claim.Claim, string) {
	t.Helper()
	c, ok := claim.Read(sessionID, time.Now())
	if !ok {
		t.Fatalf("%s was not marked", sessionID)
	}
	for _, p := range c.Placements {
		if p.ProjectID != "" || p.Repository != (config.Repository{}) || p.Repo != "" {
			t.Fatalf("%s is claimed, not marked: %+v", sessionID, c)
		}
	}
	dir, _ := claim.Dir()
	data, err := os.ReadFile(filepath.Join(dir, "claims", sessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return c, string(data)
}

// A checkout is admitted by origin alone, in any of git's URL forms, whatever its folder.
func TestAdmissionByOrigin(t *testing.T) {
	initRepo(t)
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
		_, err := Env{Cwd: root, Policy: pol, Team: "t1"}.Repo(t.Context())
		if got := err == nil; got != tc.want {
			t.Errorf("%s (%s): admitted %v, err %v", tc.folder, tc.remote, got, err)
		}
	}
}

// Outside git nothing is admitted, whatever the folder is called.
func TestAdmissionOutsideGit(t *testing.T) {
	initRepo(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(parent, "mirador-platform")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (Env{Cwd: dir, Policy: listing("github.com/miradorlabs/mirador-platform"), Team: "t1"}).Repo(t.Context()); err == nil {
		t.Fatal("a folder outside git was admitted")
	}
}

// A hook in a repository the list does not name records nothing: no spool line, no claim,
// no session state, no trailer. It only marks the session not collected, naming neither
// the team nor the repository, so the relay drops its telemetry at once.
func TestAHookOutsideTheListOnlyMarksTheSession(t *testing.T) {
	initRepo(t)
	root := namedRepo(t, "personal", "git@github.com:me/personal.git")
	hookruntest.RelayOn(t)
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) Env {
		return Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Team: "t1", Policy: fetched(listing("github.com/acme/work"))}
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
	c, raw := markOf(t, "s1")
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
	initRepo(t)
	main := namedRepo(t, "checkout-a", "git@github.com:miradorlabs/mirador-platform.git")
	ctx := t.Context()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(filepath.Dir(main), "feature")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	hookruntest.RelayOn(t)
	for i, cwd := range []string{filepath.Join(main, "src", "pkg"), filepath.Join(wt, "docs")} {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		sid := []string{"from-subdir", "from-worktree"}[i]
		env := Env{Now: time.Now(), Cwd: cwd, Team: "t1", Policy: listing("github.com/miradorlabs/mirador-platform"),
			Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + hookruntest.InJSON(cwd) + `"}`)}
		if err := startSession(ctx, env); err != nil {
			t.Fatal(err)
		}
		c, ok := claim.Read(sid, time.Now())
		if !ok || c.Repository.Origin != "github.com/miradorlabs/mirador-platform" || c.Repo != "checkout-a" {
			t.Errorf("%s: claim %+v, %v", sid, c, ok)
		}
	}
}

// A session whose next turn runs in a repository the list does not name is marked there:
// its claim's latest placement names no project and no repository, which the relay drops,
// while the earlier one stays.
func TestATurnInAnUnlistedRepositoryMarksTheClaim(t *testing.T) {
	initRepo(t)
	work := namedRepo(t, "work", "git@github.com:acme/work.git")
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	hookruntest.RelayOn(t)
	pol := listing("github.com/acme/work")
	ctx := t.Context()
	at := time.Now()
	if !ClaimFromPayload(ctx, Env{Now: at, Cwd: work, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: work}, "codex") {
		t.Fatal("the admitted turn was not claimed")
	}
	if ClaimFromPayload(ctx, Env{Now: at.Add(time.Minute), Cwd: personal, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: personal}, "codex") {
		t.Fatal("the unlisted turn claimed")
	}
	c, ok := claim.Read("thread", at.Add(time.Minute))
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
	initRepo(t)
	work := namedRepo(t, "work", "git@github.com:acme/work.git")
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	hookruntest.RelayOn(t)
	pol := fetched(listing("github.com/acme/work"))
	ctx := t.Context()
	at := time.Now()
	if ClaimFromPayload(ctx, Env{Now: at, Cwd: personal, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: personal}, "codex") {
		t.Fatal("the unlisted turn reported a claim")
	}
	markOf(t, "thread")
	if !ClaimFromPayload(ctx, Env{Now: at.Add(time.Minute), Cwd: work, Team: "t1", Policy: pol}, PayloadSession{ID: "thread", Cwd: work}, "codex") {
		t.Fatal("the listed turn was not claimed")
	}
	c, ok := claim.Read("thread", at.Add(time.Minute))
	if !ok || len(c.Placements) != 2 || c.Placements[0].ProjectID != "" || c.ProjectID != "t1" || !pol.Admits(c.Repository) {
		t.Fatalf("claim %+v, %v", c, ok)
	}
}

// Every working copy the validated policy does not admit is marked, outside git and a
// subagent's own id included; a policy not yet validated, or global mode, marks nothing.
func TestWhereAHookMarks(t *testing.T) {
	initRepo(t)
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	hookruntest.RelayOn(t)
	ctx := t.Context()
	listed := fetched(listing("github.com/acme/work"))
	mark := func(dir, sid, agent string, pol config.Policy) {
		ClaimFromPayload(ctx, Env{Now: time.Now(), Cwd: dir, Team: "t1", Policy: pol}, PayloadSession{ID: sid, AgentID: agent, Cwd: dir}, "claude-code")
	}
	mark(outside, "outside", "", listed)
	markOf(t, "outside")
	mark(personal, "parent", "child", listed)
	markOf(t, "parent")
	markOf(t, "child")
	mark(personal, "unvalidated", "", listing("github.com/acme/work"))
	if _, ok := claim.Read("unvalidated", time.Now()); ok {
		t.Error("a policy not yet validated marked a session")
	}
	global := fetched(config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"})
	mark(personal, "global", "", global)
	if c, ok := claim.Read("global", time.Now()); !ok || c.ProjectID != "p-default" || len(c.Placements) != 1 {
		t.Errorf("global mode: claim %+v, %v; want the default project's, no mark", c, ok)
	}
}

// BenchmarkClaimFromPayloadUnlisted is an agent hook in an unlisted repository once its
// session is marked: admission, then the mark's fast path.
func BenchmarkClaimFromPayloadUnlisted(b *testing.B) {
	b.Setenv("TERMA_CONFIG_DIR", b.TempDir())
	parent, _ := filepath.EvalSymlinks(b.TempDir())
	root := filepath.Join(parent, "personal")
	for _, a := range [][]string{{"init", "-q", root}, {"-C", root, "remote", "add", "origin", "git@github.com:me/personal.git"}} {
		if _, err := gitx.Git(b.Context(), parent, a...); err != nil {
			b.Fatal(err)
		}
	}
	tok, _ := claim.TokenPath()
	if err := os.MkdirAll(filepath.Dir(tok), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("local"), 0o600); err != nil {
		b.Fatal(err)
	}
	env := Env{Now: time.Now(), Cwd: root, Team: "t1", Policy: fetched(listing("github.com/acme/work"))}
	s := PayloadSession{ID: "bench", Cwd: root}
	ClaimFromPayload(b.Context(), env, s, "claude-code")
	for b.Loop() {
		ClaimFromPayload(b.Context(), env, s, "claude-code")
	}
}
