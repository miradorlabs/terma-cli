package hookrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestParsePushInput(t *testing.T) {
	a, b, z := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("0", 40)
	input := strings.Join([]string{
		"refs/heads/feature/parser " + a + " refs/heads/feature/parser " + b, // an update
		"refs/heads/new " + a + " refs/heads/new " + z,                       // a new branch
		"HEAD " + a + " refs/heads/review/x " + z,                            // HEAD to another name
		"(delete) " + z + " refs/heads/old " + b,                             // a delete sends nothing
		"refs/tags/v1 " + a + " refs/tags/v1 " + z,                           // no branch
		"garbage",
		"refs/heads/x short refs/heads/x " + b,
		"",
	}, "\n")
	got := ParsePushInput(input)
	want := []PushRef{
		{LocalRef: "refs/heads/feature/parser", LocalSHA: a, RemoteRef: "refs/heads/feature/parser", RemoteSHA: b},
		{LocalRef: "refs/heads/new", LocalSHA: a, RemoteRef: "refs/heads/new", RemoteSHA: z},
		{LocalRef: "HEAD", LocalSHA: a, RemoteRef: "refs/heads/review/x", RemoteSHA: z},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// pushRepo is a checkout with a bare remote "backup", and main already pushed to it.
type pushRepo struct {
	t            *testing.T
	root, remote string
	stateDir     string
	sp           *spool.Spool
	team         string
}

func newPushRepo(t *testing.T) *pushRepo {
	t.Helper()
	root := hookruntest.InitRepo(t)
	remote := filepath.Join(t.TempDir(), "backup.git")
	p := &pushRepo{t: t, root: root, remote: remote, stateDir: t.TempDir(), team: hookruntest.Team}
	p.git("init", "-q", "--bare", "-b", "main", remote)
	p.git("remote", "add", "backup", remote)
	p.git("commit", "-q", "--allow-empty", "-m", "init")
	p.git("push", "-q", "backup", "main")
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.sp = sp
	return p
}

func (p *pushRepo) git(args ...string) string {
	p.t.Helper()
	out, err := gitx.Git(context.Background(), p.root, args...)
	if err != nil {
		p.t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func (p *pushRepo) sha(rev string) string { return p.git("rev-parse", rev) }

// push runs what git runs: pre-push with line on stdin, then the push itself, then the
// report once git has exited. It returns the events and whether git's push succeeded.
func (p *pushRepo) push(line string, args ...string) ([]spool.Event, bool) {
	p.t.Helper()
	// A stand-in for git push's process, alive while the push runs.
	fakeGit := exec.Command("sleep", "60")
	if err := fakeGit.Start(); err != nil {
		p.t.Fatal(err)
	}
	gitPushPID = func() int { return fakeGit.Process.Pid }
	var recorded string
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"backup", p.remote},
		Stdin: strings.NewReader(line + "\n"), Spool: p.sp, Team: p.team, Policy: hookruntest.Admitting(p.root),
		AwaitPush: func(path string) { recorded = path }}
	if err := PrePush(context.Background(), env); err != nil {
		p.t.Fatal(err)
	}
	if recorded == "" {
		p.t.Fatal("pre-push recorded no push")
	}
	// The reporter waits while git push runs, as the detached one does.
	done := make(chan struct{})
	go func() { AwaitPush(context.Background(), env, recorded); close(done) }()
	_, err := gitx.Git(context.Background(), p.root, append([]string{"push", "-q", "backup"}, args...)...)
	select {
	case <-done:
		p.t.Fatal("the push was reported while git push still ran")
	case <-time.After(2 * pushPoll):
	}
	_ = fakeGit.Process.Kill()
	_ = fakeGit.Wait()
	<-done
	if _, statErr := os.Stat(recorded); statErr == nil {
		p.t.Error("the push record was left behind")
	}
	return hookruntest.Named(hookruntest.Spooled(p.t, p.sp), semconv.TermaPushEvent), err == nil
}

func pushLine(local, localSHA, remoteRef, remoteSHA string) string {
	return local + " " + localSHA + " " + remoteRef + " " + remoteSHA
}

const zero = "0000000000000000000000000000000000000000"

func strs(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, s := range list {
			out = append(out, s.(string))
		}
	}
	return out
}

func TestPushReportsEachBranchOnceGitPushExits(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)

	// A new branch with one stamped commit: only that commit, since main is on the remote.
	p.git("checkout", "-q", "-b", "feature")
	p.git("commit", "-q", "--allow-empty", "-m", "agent work\n\nAgent-Session-Id: sess-1\nAgent-Tool: claude")
	c1 := p.sha("HEAD")
	events, ok := p.push(pushLine("refs/heads/feature", c1, "refs/heads/feature", zero), "feature")
	if !ok || len(events) != 1 {
		t.Fatalf("new branch: pushed %v, events %+v", ok, events)
	}
	a := events[0].Attrs
	if a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusTrackingRefUpdated || a[semconv.TermaPushRangeKey] != semconv.TermaPushRangeNewBranch ||
		!slices.Equal(strs(a[semconv.TermaPushCommitsKey]), []string{c1}) || hookruntest.Num(a[semconv.TermaPushCommitCountKey]) != 1 ||
		a[semconv.TermaPushCommitsTruncatedKey] != false || !slices.Equal(strs(a[semconv.TermaPushSessionIDsKey]), []string{"sess-1"}) ||
		events[0].SessionID != "sess-1" || a[semconv.TermaPushRemoteNameKey] != "backup" || a[semconv.TermaPushRemoteRefKey] != "refs/heads/feature" ||
		a[semconv.TermaPushNewRevisionKey] != c1 || a[semconv.TermaPushOldRevisionKey] != nil || a[semconv.TermaPushForcedKey] != nil ||
		a[AttrProjectID] != hookruntest.Team || a[semconv.VCSRepositoryURLFullKey] == nil {
		t.Errorf("new branch: %+v", a)
	}
	// The remote's local path is no URL to send.
	if _, ok := a[semconv.TermaPushRemoteURLKey]; ok {
		t.Errorf("a local path went out as the remote's URL: %v", a[semconv.TermaPushRemoteURLKey])
	}

	// A fast-forward: the one new commit, not forced; an unstamped push has no session.
	p.git("commit", "-q", "--allow-empty", "-m", "by hand")
	c2 := p.sha("HEAD")
	events, _ = p.push(pushLine("refs/heads/feature", c2, "refs/heads/feature", c1), "feature")
	a = events[0].Attrs
	if ids, ok := a[semconv.TermaPushSessionIDsKey].([]any); !ok || len(ids) != 0 {
		t.Errorf("an unstamped push's session ids are %#v, not an empty list", a[semconv.TermaPushSessionIDsKey])
	}
	if a[semconv.TermaPushRangeKey] != semconv.TermaPushRangeUpdate || !slices.Equal(strs(a[semconv.TermaPushCommitsKey]), []string{c2}) ||
		a[semconv.TermaPushForcedKey] != false || a[semconv.TermaPushOldRevisionKey] != c1 || events[0].SessionID != "" ||
		len(strs(a[semconv.TermaPushSessionIDsKey])) != 0 || a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusTrackingRefUpdated {
		t.Errorf("fast-forward: %+v", a)
	}

	// A force-push after an amend: the amended commit, forced.
	p.git("commit", "-q", "--amend", "--allow-empty", "-m", "by hand, amended")
	c2b := p.sha("HEAD")
	events, _ = p.push(pushLine("refs/heads/feature", c2b, "refs/heads/feature", c2), "--force", "feature")
	a = events[0].Attrs
	if !slices.Equal(strs(a[semconv.TermaPushCommitsKey]), []string{c2b}) || a[semconv.TermaPushForcedKey] != true ||
		a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusTrackingRefUpdated {
		t.Errorf("force-push: %+v", a)
	}

	// HEAD to a differently named branch: its commits are all on the remote already.
	events, _ = p.push(pushLine("HEAD", c2b, "refs/heads/review/x", zero), "HEAD:refs/heads/review/x")
	a = events[0].Attrs
	if a[semconv.TermaPushLocalRefKey] != "HEAD" || a[semconv.TermaPushRemoteRefKey] != "refs/heads/review/x" ||
		hookruntest.Num(a[semconv.TermaPushCommitCountKey]) != 0 || a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusTrackingRefUpdated {
		t.Errorf("renamed push: %+v", a)
	}

	// A dry run moves nothing.
	p.git("commit", "-q", "--allow-empty", "-m", "not pushed")
	c3 := p.sha("HEAD")
	events, _ = p.push(pushLine("refs/heads/feature", c3, "refs/heads/feature", c2b), "--dry-run", "feature")
	if a = events[0].Attrs; a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusUnknown {
		t.Errorf("dry run: %+v", a)
	}
}

// A rejected push never reads as landed, even when the old commit is unknown here.
func TestARejectedPushIsUnknown(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	// Someone else moves the remote's main.
	other := filepath.Join(t.TempDir(), "other")
	if _, err := gitx.Git(context.Background(), p.root, "clone", "-q", p.remote, other); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-c", "user.email=o@example.com", "-c", "user.name=O", "commit", "-q", "--allow-empty", "-m", "theirs"},
		{"push", "-q", "origin", "main"},
	} {
		if _, err := gitx.Git(context.Background(), other, args...); err != nil {
			t.Fatal(err)
		}
	}
	theirs, _ := gitx.Git(context.Background(), other, "rev-parse", "HEAD")
	p.git("commit", "-q", "--allow-empty", "-m", "mine")
	mine := p.sha("HEAD")
	events, pushed := p.push(pushLine("refs/heads/main", mine, "refs/heads/main", theirs), "main")
	if pushed {
		t.Fatal("the non-fast-forward push was accepted")
	}
	a := events[0].Attrs
	if a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusUnknown || a[semconv.TermaPushRangeKey] != semconv.TermaPushRangeFallback ||
		!slices.Equal(strs(a[semconv.TermaPushCommitsKey]), []string{mine}) || a[semconv.TermaPushForcedKey] != nil {
		t.Errorf("rejected: %+v", a)
	}
}

// A remote-tracking branch already at the pushed commit shows nothing about this push.
func TestAStaleTrackingBranchIsNoEvidence(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	main := p.sha("main")
	events, _ := p.push(pushLine("refs/heads/main", main, "refs/heads/main", zero), "main")
	if a := events[0].Attrs; a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusUnknown {
		t.Errorf("stale: %+v", a)
	}
}

// A push to a URL has no remote-tracking branch, and its URL is never a remote's name.
func TestAPushToAURLIsUnknown(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	p.git("commit", "-q", "--allow-empty", "-m", "more")
	head := p.sha("HEAD")
	gitPushPID = func() int { return 0 }
	var recorded string
	url := "https://user:token@example.com/acme/r.git"
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{url, url},
		Stdin: strings.NewReader(pushLine("refs/heads/main", head, "refs/heads/main", zero) + "\n"), Spool: p.sp,
		Team: p.team, Policy: hookruntest.Admitting(p.root), AwaitPush: func(path string) { recorded = path }}
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	AwaitPush(context.Background(), env, recorded)
	events := hookruntest.Named(hookruntest.Spooled(t, p.sp), semconv.TermaPushEvent)
	a := events[0].Attrs
	// No remote to go by: what no remote-tracking branch had, main's first commit excluded.
	if a[semconv.TermaPushStatusKey] != semconv.TermaPushStatusUnknown || a[semconv.TermaPushRemoteNameKey] != nil ||
		a[semconv.TermaPushRemoteURLKey] != "https://example.com/acme/r" || a[semconv.TermaPushRangeKey] != semconv.TermaPushRangeFallback ||
		!slices.Equal(strs(a[semconv.TermaPushCommitsKey]), []string{head}) {
		t.Errorf("by URL: %+v", a)
	}
}

// A record no AwaitPush finished is reported by the sweep, once.
func TestSweepReportsAnAbandonedPush(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	p.git("commit", "-q", "--allow-empty", "-m", "more")
	head := p.sha("HEAD")
	gitPushPID = func() int { return 0 }
	var recorded string
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"backup", p.remote},
		Stdin: strings.NewReader(pushLine("refs/heads/main", head, "refs/heads/main", p.sha("backup/main")) + "\n"), Spool: p.sp,
		Team: p.team, Policy: hookruntest.Admitting(p.root), AwaitPush: func(path string) { recorded = path }}
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	SweepPushes(context.Background(), env)
	if _, err := os.Stat(recorded); err != nil {
		t.Fatal("a fresh record was swept")
	}
	// The flush that sweeps may have been started by another repository's hook, whose git
	// directory it inherits: the record's repository is still the one read.
	t.Setenv("GIT_DIR", filepath.Join(hookruntest.InitRepo(t), ".git"))
	old := time.Now().Add(-pushAbandoned - time.Minute)
	if err := os.Chtimes(recorded, old, old); err != nil {
		t.Fatal(err)
	}
	SweepPushes(context.Background(), env)
	SweepPushes(context.Background(), env)
	if events := hookruntest.Named(hookruntest.Spooled(t, p.sp), semconv.TermaPushEvent); len(events) != 1 {
		t.Fatalf("swept events: %+v", events)
	}
	if _, err := os.Stat(recorded); err == nil {
		t.Error("the swept record was left behind")
	}
}

// Nothing to report records nothing: an up-to-date push gives pre-push no lines.
func TestPrePushWithNothingToReport(t *testing.T) {
	p := newPushRepo(t)
	called := false
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"backup", p.remote}, Stdin: strings.NewReader(""),
		Spool: p.sp, Team: p.team, Policy: hookruntest.Admitting(p.root), AwaitPush: func(string) { called = true }}
	if err := PrePush(context.Background(), env); err != nil || called {
		t.Fatalf("err %v, awaited %v", err, called)
	}
	if entries, _ := os.ReadDir(filepath.Join(p.stateDir, PushesDir)); len(entries) > 0 {
		t.Errorf("recorded %v", entries)
	}
}

// A record taken and never finished, its reporter killed, is reported by the sweep.
func TestSweepReportsATakenPushItsReporterLeft(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	p.git("commit", "-q", "--allow-empty", "-m", "more")
	head := p.sha("HEAD")
	gitPushPID = func() int { return 0 }
	var recorded string
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"backup", p.remote},
		Stdin: strings.NewReader(pushLine("refs/heads/main", head, "refs/heads/main", p.sha("backup/main")) + "\n"), Spool: p.sp,
		Team: p.team, Policy: hookruntest.Admitting(p.root), AwaitPush: func(path string) { recorded = path }}
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	taken := recorded + takenSuffix
	if err := os.Rename(recorded, taken); err != nil {
		t.Fatal(err)
	}
	SweepPushes(context.Background(), env)
	if _, err := os.Stat(taken); err != nil {
		t.Fatal("a record still being reported was swept")
	}
	old := time.Now().Add(-pushAbandoned - time.Minute)
	if err := os.Chtimes(taken, old, old); err != nil {
		t.Fatal(err)
	}
	SweepPushes(context.Background(), env)
	events := hookruntest.Named(hookruntest.Spooled(t, p.sp), semconv.TermaPushEvent)
	if len(events) != 1 || !slices.Equal(strs(events[0].Attrs[semconv.TermaPushCommitsKey]), []string{head}) {
		t.Fatalf("swept events: %+v", events)
	}
	if left, _ := os.ReadDir(filepath.Join(p.stateDir, PushesDir)); len(left) > 0 {
		t.Errorf("left %v", left)
	}
}

// The sessions are those of the commits listed, and both lists are empty, never nil.
func TestPushedListsNameOnlyTheListedCommitsSessions(t *testing.T) {
	commits := make([]gitx.PushedCommit, MaxPushCommits+1)
	for i := range commits {
		commits[i].SHA = strings.Repeat("a", 40)
	}
	commits[0].Sessions = []string{"sess-listed", "sess-listed", "../bad"}
	commits[MaxPushCommits].Sessions = []string{"sess-beyond"}
	shas, sessions := pushedLists(commits)
	if len(shas) != MaxPushCommits || !slices.Equal(sessions, []string{"sess-listed"}) {
		t.Errorf("%d shas, sessions %q", len(shas), sessions)
	}
	if shas, sessions := pushedLists(nil); shas == nil || sessions == nil {
		t.Errorf("nil lists: %#v %#v", shas, sessions)
	}
}

// A remote named for its own location is still a remote: git passes it as both arguments.
func TestARemoteNamedForItsLocationIsARemote(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	p := newPushRepo(t)
	// git resolves the remote's path from the checkout, where pre-push runs.
	p.git("init", "-q", "--bare", "-b", "main", filepath.Join(p.root, "same.git"))
	p.git("remote", "add", "same.git", "same.git")
	p.git("commit", "-q", "--allow-empty", "-m", "more")
	head := p.sha("HEAD")
	gitPushPID = func() int { return 0 }
	var recorded string
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Args: []string{"same.git", "same.git"},
		Stdin: strings.NewReader(pushLine("refs/heads/main", head, "refs/heads/main", zero) + "\n"), Spool: p.sp,
		Team: p.team, Policy: hookruntest.Admitting(p.root), AwaitPush: func(path string) { recorded = path }}
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	rec, err := readPush(recorded)
	if err != nil || rec.Remote != "same.git" || rec.Refs[0].Tracking != "refs/remotes/same.git/main" {
		t.Fatalf("record: %+v, %v", rec, err)
	}
}
