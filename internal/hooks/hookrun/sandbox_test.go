package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Two people's accounts, as their sign-ins name them.
const personA, personB = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"

// gitStore is p's session store, its manifests named for account.
func gitStore(p *pushRepo, account string) *session.Store {
	s := session.Open(filepath.Join(p.root, ".git", project.GitStoreDir))
	s.Bind(account)
	return s
}

// sandboxedHook is a git hook in an agent's sandbox: it can't read the sign-in, and neither
// its spool nor its state directory takes a write, so whatever it keeps is parked.
func sandboxedHook(t *testing.T, p *pushRepo) Env {
	t.Helper()
	dir, state := t.TempDir(), t.TempDir()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(sendas.AccountUnreadable)
	readOnly(t, dir, state)
	return Env{StateDir: state, Now: time.Now(), Cwd: p.root, Spool: sp, Team: p.team, Policy: hookruntest.Admitting(p.root),
		Account: sendas.AccountUnreadable, AwaitPush: func(string) {}}
}

// readOnly makes dirs refuse a write, as an agent's sandbox does, until the test ends; it
// skips the test where a directory's mode can't (modeIgnored).
func readOnly(t *testing.T, dirs ...string) {
	t.Helper()
	if why := modeIgnored(); why != "" {
		t.Skip(why)
	}
	chmodReadOnly(t, dirs...)
}

// chmodReadOnly sets dirs' mode to refuse a write until the test ends, where the mode is
// kept (modeIgnored).
func chmodReadOnly(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
	}
}

// modeIgnored is why a directory whose mode forbids a write takes one anyway here, or ""
// where the mode is kept. Where it is ignored, a sandboxed hook would write its state
// directory and spool as any other hook does, and the sandbox's path would go untested.
func modeIgnored() string {
	switch {
	case runtime.GOOS == "windows":
		return "Windows writes a directory whose mode forbids it"
	case os.Geteuid() == 0:
		return "root writes a directory its mode forbids"
	}
	return ""
}

// signedIn is a hook outside the sandbox signed in as account, whose AwaitPush reports at once.
func signedIn(t *testing.T, p *pushRepo, account string) (Env, string) {
	t.Helper()
	dir := t.TempDir()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(account)
	env := Env{StateDir: p.stateDir, Now: time.Now(), Cwd: p.root, Spool: sp, Team: p.team, Policy: hookruntest.Admitting(p.root), Account: account}
	env.AwaitPush = func(path string) { AwaitPush(context.Background(), env, path) }
	return env, dir
}

// commit commits files, each written anew, stamped with sessions (claude-code's), and runs
// post-commit under env. It returns the commit.
func commit(t *testing.T, p *pushRepo, env Env, files []string, sessions ...string) string {
	t.Helper()
	msg := "work\n\n"
	for _, s := range sessions {
		msg += "Agent-Session-Id: " + s + "\n"
	}
	msg += "Agent-Tool: claude-code\n"
	for _, f := range files {
		hookruntest.WriteFile(t, p.root, f, f+time.Now().String())
		p.git("add", f)
	}
	p.git("commit", "-q", "-m", msg)
	if err := PostCommit(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	return p.sha("HEAD")
}

// prePush runs pre-push under env for main pushed up to sha.
func prePush(t *testing.T, p *pushRepo, env Env, sha string) {
	t.Helper()
	env.Args = []string{"backup", p.remote}
	env.Stdin = strings.NewReader(pushLine("refs/heads/main", sha, "refs/heads/main", p.sha("backup/main")) + "\n")
	if err := PrePush(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// lanes is the lane each queued event is in, by its name and the commit it records, or for a
// push the commits it pushed.
func lanes(t *testing.T, sp *spool.Spool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ev := range hookruntest.Spooled(t, sp) {
		what, _ := ev.Attrs[semconv.VCSRefHeadRevisionKey].(string)
		if ev.Name == semconv.TermaPushEvent {
			what = strings.Join(strs(ev.Attrs[semconv.TermaPushCommitsKey]), ",")
		}
		lane := ev.Account
		if ev.NoPerson {
			lane = sendas.NoPersonLane
		}
		out[ev.Name+" "+what] = lane
	}
	return out
}

func lossesIn(t *testing.T, dir string) map[string]int {
	t.Helper()
	l, err := spool.ReadLosses(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return l.Counts
}

// Two people's sessions in one repository: what a sandboxed git hook records of A's session
// is A's, of B's is B's, whoever unparks it, and a commit or push of both is no one's: lost,
// and counted ambiguous_push once the next hook outside the sandbox takes the counts in.
func TestASandboxedRecordIsInItsSessionsLane(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go", "both-a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := gitStore(p, personB).Touch(session.Session{ID: "sess-b", Tool: "claude-code"}, []string{"b.go", "both-b.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	sandboxed := sandboxedHook(t, p)
	onlyA := commit(t, p, sandboxed, []string{"a.go"}, "sess-a")
	prePush(t, p, sandboxed, onlyA)
	onlyB := commit(t, p, sandboxed, []string{"b.go"}, "sess-b")
	both := commit(t, p, sandboxed, []string{"both-a.go", "both-b.go"}, "sess-a", "sess-b")
	prePush(t, p, sandboxed, both)

	// Someone else entirely unparks it all.
	outside, dir := signedIn(t, p, "cccccccccccccccc")
	if _, err := outside.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := lanes(t, outside.Spool)
	want := map[string]string{
		semconv.TermaCommitEvent + " " + onlyA: personA,
		semconv.TermaCommitEvent + " " + onlyB: personB,
		semconv.TermaPushEvent + " " + onlyA:   personA,
	}
	if len(got) != len(want) {
		t.Errorf("queued %v; want %v", got, want)
	}
	for k, lane := range want {
		if got[k] != lane {
			t.Errorf("%s in %q; want %q", k, got[k], lane)
		}
	}
	if l := lossesIn(t, dir); l[sendas.LostAmbiguousPush] != 2 || len(l) != 1 {
		t.Errorf("losses = %v; want the commit and the push of both counted ambiguous_push", l)
	}
}

// Outside a sandbox a git hook's record is its own sign-in's, its collector's, and kept only
// if no session it is of is another person's: a commit of A's session under A is A's, one of
// B's or of both under A is lost. A session whose manifest names no one does not object.
func TestARecordIsItsHooksOnlyWhereItsSessionsAgree(t *testing.T) {
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go", "both-a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := gitStore(p, personB).Touch(session.Session{ID: "sess-b", Tool: "claude-code"}, []string{"b.go", "both-b.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := gitStore(p, "").Touch(session.Session{ID: "sess-old", Tool: "claude-code"}, []string{"old.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	asA, dir := signedIn(t, p, personA)
	onlyA := commit(t, p, asA, []string{"a.go"}, "sess-a")
	commit(t, p, asA, []string{"b.go"}, "sess-b")
	commit(t, p, asA, []string{"both-a.go", "both-b.go"}, "sess-a", "sess-b")
	old := commit(t, p, asA, []string{"old.go"}, "sess-old")
	got := lanes(t, asA.Spool)
	if len(got) != 2 || got[semconv.TermaCommitEvent+" "+onlyA] != personA || got[semconv.TermaCommitEvent+" "+old] != personA {
		t.Errorf("queued %v; want only the commits of A's session, and of no one's, under A", got)
	}
	if l := lossesIn(t, dir); l[sendas.LostAmbiguousPush] != 2 || len(l) != 1 {
		t.Errorf("losses = %v; want B's commit and both's counted ambiguous_push", l)
	}
}

// A manifest with no account, as an older terma wrote it, gives a sandboxed record no lane:
// it is lost, and counted, never put in the lane of whoever is signed in now, and a write
// under someone's sign-in since never gives the manifest one.
func TestAManifestWithNoAccountGivesNoLane(t *testing.T) {
	p := newPushRepo(t)
	sess := session.Session{ID: "sess-old", Tool: "claude-code"}
	if err := gitStore(p, "").Touch(sess, []string{"old.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := gitStore(p, personA).Touch(sess, []string{"new.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	commit(t, p, sandboxedHook(t, p), []string{"old.go", "new.go"}, "sess-old")
	asA, dir := signedIn(t, p, personA)
	if _, err := asA.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lanes(t, asA.Spool); len(got) != 0 {
		t.Errorf("queued %v; want the commit of a session that names no one lost", got)
	}
	if l := lossesIn(t, dir); l[sendas.LostAmbiguousPush] != 1 || len(l) != 1 {
		t.Errorf("losses = %v; want it counted ambiguous_push", l)
	}
	ms, err := gitStore(p, "").Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != "" {
		t.Errorf("manifests %+v, %v; want the session still no one's", ms, err)
	}
}

// A sign-in switch mid-session leaves the session its first account's: B's hook records
// nothing of A's session and spools nothing of it, but counts its event account_conflict.
func TestASignInSwitchNeverRetagsASession(t *testing.T) {
	p := newPushRepo(t)
	sess := session.Session{ID: "sess-a", Tool: "claude-code"}
	asA, _ := signedIn(t, p, personA)
	r, err := asA.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asA.Touch(r, sess, "Edit", []string{"a.go"}, nil)
	asB, dir := signedIn(t, p, personB)
	r, err = asB.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asB.Touch(r, sess, "Edit", []string{"b.go"}, nil)
	if evs := hookruntest.Spooled(t, asB.Spool); len(evs) != 0 {
		t.Errorf("B spooled %s of A's session", hookruntest.Names(evs))
	}
	if l := lossesIn(t, dir); l[sendas.LostAccountConflict] != 1 || len(l) != 1 {
		t.Errorf("losses = %v; want B's touch counted account_conflict", l)
	}
	ms, err := r.Store.Manifests()
	if err != nil || len(ms) != 1 || ms[0].Account != personA || len(ms[0].Files) != 1 {
		t.Fatalf("manifests %+v, %v; want A's session, with A's file alone", ms, err)
	}
}

// An older terma's hook beside this one: what it parked (in .git/terma's events and pushes)
// is queued where it queues, for the legacy decision to take in, and what this one parks is
// under names it never reads.
func TestAnOlderTermasParkedRecordsAreItsBacklog(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	older := session.Open(filepath.Join(p.root, ".git", project.GitStoreDir))
	if err := older.Park(legacyParkedEvents, []byte(`{"name":"older-event","time":"2026-11-20T00:00:00Z"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	p.git("commit", "-q", "--allow-empty", "-m", "older push")
	rec := `{"id":"older-push","time":"2026-11-20T00:00:00Z","root":"` + p.root + `","git_dir":"` + filepath.Join(p.root, ".git") +
		`","remote":"backup","refs":[{"local_ref":"refs/heads/main","local_sha":"` + p.sha("HEAD") + `","remote_ref":"refs/heads/main","remote_sha":"` +
		p.sha("backup/main") + `"}],"event":{"name":"` + semconv.TermaPushEvent + `","time":"2026-11-20T00:00:00Z"}}` + "\n"
	if err := older.Park(legacyParkedPushes, []byte(rec)); err != nil {
		t.Fatal(err)
	}
	// This version parks a tagged event; an older terma's unpark finds nothing.
	sandboxed := sandboxedHook(t, p)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ours := commit(t, p, sandboxed, []string{"a.go"}, "sess-a")
	for _, name := range []string{legacyParkedEvents, legacyParkedPushes} {
		var olderSaw []byte
		if err := older.Unpark(name, func(lines []byte) error {
			olderSaw = append(olderSaw, lines...)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(olderSaw), personA) {
			t.Errorf("an older terma's %s unparked this one's %s", name, olderSaw)
		}
		// Put back what the older terma's own unpark took, as though it never ran.
		if len(olderSaw) > 0 {
			if err := older.Park(name, olderSaw); err != nil {
				t.Fatal(err)
			}
		}
	}
	outside, dir := signedIn(t, p, personB)
	if _, err := outside.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lanes(t, outside.Spool); len(got) != 1 || got[semconv.TermaCommitEvent+" "+ours] != personA {
		t.Errorf("queued %v; want only this version's commit, in its own lane", got)
	}
	if err := outside.Spool.IngestLegacy(personA, false); err != nil {
		t.Fatal(err)
	}
	got := lanes(t, outside.Spool)
	if got["older-event "] != personA || got[semconv.TermaPushEvent+" "+p.sha("HEAD~1")] != personA || len(got) != 2 {
		t.Errorf("after the legacy decision, queued %v; want the older terma's event and push under it", got)
	}
	if _, err := os.Stat(filepath.Join(dir, spool.LegacyEventsFile)); err == nil {
		t.Error("the older queue was not taken in")
	}
}

// What a sandboxed hook lost is counted where it can write, and the next hook outside the
// sandbox moves the counts into the spool's: once, however many hooks follow, and once even
// when a move is retried after it wrote its copy.
func TestSandboxedLossesAreCountedOnce(t *testing.T) {
	p := newPushRepo(t)
	sandboxed := sandboxedHook(t, p)
	commit(t, p, sandboxed, []string{"x.go"}, "sess-unknown")
	commit(t, p, sandboxed, []string{"y.go"}, "sess-unknown")
	parking := session.Open(filepath.Join(p.root, ".git", project.GitStoreDir))
	// A move that copied, then crashed before removing what it copied.
	outside, dir := signedIn(t, p, personA)
	if err := parking.Take(parkedLosses, func(path, id string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		copied := filepath.Join(t.TempDir(), "copy")
		if err := os.WriteFile(copied, data, 0o600); err != nil {
			return err
		}
		if err := outside.Spool.ImportLosses(copied, id); err != nil {
			return err
		}
		return os.ErrDeadlineExceeded
	}); err == nil {
		t.Fatal("the crashed move went on")
	}
	for range 3 {
		if _, err := outside.Repo(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if l := lossesIn(t, dir); l[sendas.LostAmbiguousPush] != 2 || len(l) != 1 {
		t.Errorf("losses = %v; want the sandbox's two, once", l)
	}
}

// Signed out is signed out in a sandbox too: a sandboxed hook that reads no sign-in at all
// loses its commit and push, counted signed_out, never putting them in its session's lane.
func TestASignedOutSandboxedHookTakesNoSessionsLane(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	signedOut := sandboxedHook(t, p)
	signedOut.Account = ""
	signedOut.Spool.Bind("")
	prePush(t, p, signedOut, commit(t, p, signedOut, []string{"a.go"}, "sess-a"))
	outside, dir := signedIn(t, p, personA)
	if _, err := outside.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lanes(t, outside.Spool); len(got) != 0 {
		t.Errorf("queued %v; want nothing signed out collected", got)
	}
	if l := lossesIn(t, dir); l[sendas.LostSignedOut] != 2 || len(l) != 1 {
		t.Errorf("losses = %v; want the commit and the push counted signed_out", l)
	}
}

// A tool call a hook first expects, then reports, is one event: lost to a conflict, it is
// counted once.
func TestAConflictIsCountedOncePerEvent(t *testing.T) {
	p := newPushRepo(t)
	sess := session.Session{ID: "sess-a", Tool: "codex"}
	if err := gitStore(p, personA).Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	asB, dir := signedIn(t, p, personB)
	r, err := asB.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asB.Expect(r, sess, []string{"b.go"})
	asB.Touch(r, sess, "apply_patch", []string{"b.go"}, nil)
	if l := lossesIn(t, dir); l[sendas.LostAccountConflict] != 1 {
		t.Errorf("losses = %v; want one account_conflict", l)
	}
}

// A session stamped with no agent has no manifest to name an account, in a push as in a
// commit: a sandboxed push of it with A's session is no one's.
func TestASandboxedPushOfASessionWithNoAgentIsNoOnes(t *testing.T) {
	defer func(f func() int) { gitPushPID = f }(gitPushPID)
	gitPushPID = func() int { return 0 }
	p := newPushRepo(t)
	if err := gitStore(p, personA).Touch(session.Session{ID: "sess-a", Tool: "claude-code"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	sandboxed := sandboxedHook(t, p)
	commit(t, p, sandboxed, []string{"a.go"}, "sess-a")
	p.git("commit", "-q", "--allow-empty", "-m", "work\n\nAgent-Session-Id: sess-x\n")
	prePush(t, p, sandboxed, p.sha("HEAD"))
	outside, _ := signedIn(t, p, personA)
	if _, err := outside.Repo(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name := range lanes(t, outside.Spool) {
		if strings.HasPrefix(name, semconv.TermaPushEvent) {
			t.Errorf("queued the push %s; want it no one's", name)
		}
	}
}

// Another person's session sends nothing whichever checkout its paths are in: B's touch of
// A's session, of a file in a linked worktree alone, is counted lost, not reported under B.
func TestATouchOfAnotherPersonsSessionInASiblingSendsNothing(t *testing.T) {
	p := newPushRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	p.git("worktree", "add", "-q", wt)
	sess := session.Session{ID: "sess-a", Tool: "claude-code"}
	if err := gitStore(p, personA).Touch(sess, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	asB, dir := signedIn(t, p, personB)
	r, err := asB.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asB.Touch(r, sess, "Edit", []string{filepath.Join(wt, "w.go")}, nil)
	asB.TouchGuessed(r, sess, "Read", []string{filepath.Join(wt, "w.go")}, nil)
	if evs := hookruntest.Spooled(t, asB.Spool); len(evs) != 0 {
		t.Errorf("B spooled %s of A's session", hookruntest.Names(evs))
	}
	if l := lossesIn(t, dir); l[sendas.LostAccountConflict] != 2 {
		t.Errorf("losses = %v; want both touches counted account_conflict", l)
	}
}
