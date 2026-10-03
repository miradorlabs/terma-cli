package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	return config.Policy{Mode: config.ModeRepo, Folders: entries}
}

// A git checkout is admitted by its folder or origin's repository name, an owner/name
// entry only by origin's.

func TestAdmissionByFolderOrOrigin(t *testing.T) {
	initRepo(t)
	for _, tc := range []struct {
		folder, remote string
		pol            config.Policy
		want           bool
	}{
		{"checkout-a", "git@github.com:miradorlabs/mirador-platform.git", listing("mirador-platform"), true},
		{"checkout-a", "https://github.com/miradorlabs/mirador-platform", listing("MiradorLabs/Mirador-Platform"), true},
		{"checkout-a", "ssh://git@github.com/miradorlabs/mirador-platform.git", listing("checkout-a"), true},
		{"mirador-platform", "git@github.com:acme/other.git", listing("mirador-platform"), true},
		{"mirador-platform", "git@github.com:acme/other.git", listing("acme/other"), true},
		{"mirador-platform", "git@github.com:acme/other.git", listing("acme/mirador-platform"), false},
		{"sales", "", listing("sales"), true},
		{"sales", "", listing("acme/sales"), false},
		{"sales", "", listing("web"), false},
	} {
		root := namedRepo(t, tc.folder, tc.remote)
		_, err := Env{Cwd: root, Policy: tc.pol, Team: "t1"}.Repo(t.Context())
		if got := err == nil; got != tc.want {
			t.Errorf("%s (%s) under %v: admitted %v, err %v", tc.folder, tc.remote, tc.pol.Folders, got, err)
		}
	}
}

// Outside Git a folder is admitted by its name or any parent's below the home directory.
func TestAdmissionOutsideGitWalksUpTheFolders(t *testing.T) {
	initRepo(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, "clients", "acme", "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for entry, want := range map[string]bool{"notes": true, "acme": true, "clients": true, filepath.Base(home): false, "other": false} {
		if _, err := (Env{Cwd: dir, Policy: listing(entry), Team: "t1"}).Repo(t.Context()); (err == nil) != want {
			t.Errorf("entry %q: err %v, want admitted %v", entry, err, want)
		}
	}
}

// A hook in a repository the list does not name writes nothing: no spool line, no claim,
// no session state, no trailer.
func TestAHookOutsideTheListWritesNothing(t *testing.T) {
	initRepo(t)
	root := namedRepo(t, "personal", "git@github.com:me/personal.git")
	hookruntest.RelayOn(t)
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) Env {
		return Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Team: "t1", Policy: listing("work")}
	}
	ctx := t.Context()
	if err := startSession(ctx, env(`{"session_id":"s1","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "a.txt", "a\n")
	if err := editFile(ctx, env(`{"session_id":"s1","cwd":"`+root+`","tool_name":"Edit","tool_input":{"file_path":"`+filepath.Join(root, "a.txt")+`"}}`)); err != nil {
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
	if _, ok := claim.Read("s1", time.Now()); ok {
		t.Fatal("the session was claimed")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "terma")); !os.IsNotExist(err) {
		t.Fatalf("session state was written: %v", err)
	}
}

// A session started in a subdirectory, or in a linked worktree whose .git is a file, is
// its checkout's, admitted by origin's repository name.
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
	// The worktree's root folder is its own: the main checkout's name does not admit it.
	if _, err := (Env{Cwd: wt, Policy: listing("checkout-a"), Team: "t1"}).Repo(ctx); err == nil {
		t.Fatal("the main checkout's folder name admitted a linked worktree")
	}
	for i, cwd := range []string{filepath.Join(main, "src", "pkg"), filepath.Join(wt, "docs")} {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		sid := []string{"from-subdir", "from-worktree"}[i]
		env := Env{Now: time.Now(), Cwd: cwd, Team: "t1", Policy: listing("mirador-platform"),
			Stdin: strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + cwd + `"}`)}
		if err := startSession(ctx, env); err != nil {
			t.Fatal(err)
		}
		c, ok := claim.Read(sid, time.Now())
		if !ok || !slices.Contains(c.Repository.Names, "mirador-platform") || c.Repository.Path != "miradorlabs/mirador-platform" || c.Repo != "checkout-a" {
			t.Errorf("%s: claim %+v, %v", sid, c, ok)
		}
	}
}

// A session whose next turn runs in a repository the list does not name is withdrawn
// there: its claim's latest placement names that repository, which the relay drops, while
// the earlier one stays.
func TestATurnInAnUnlistedRepositoryWithdrawsTheClaim(t *testing.T) {
	initRepo(t)
	work := namedRepo(t, "work", "git@github.com:acme/work.git")
	personal := namedRepo(t, "personal", "git@github.com:me/personal.git")
	hookruntest.RelayOn(t)
	pol := listing("work")
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
	if pol.Admits(c.Repository) || !pol.Admits(c.Placements[0].Repository) {
		t.Fatalf("placements %+v: want the unlisted one latest, the admitted one kept", c.Placements)
	}
}
