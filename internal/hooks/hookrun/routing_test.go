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
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const testAccountA = "1111111111111111"
const testAccountB = "2222222222222222"

func routedState(t *testing.T, stateDir, account string, teams []routing.Team, bind map[string]string) *Routing {
	t.Helper()
	st, err := routing.Create(stateDir, "default", account, func(s *routing.State) error {
		for _, tm := range teams {
			if _, err := s.Select(tm); err != nil {
				return err
			}
		}
		for k, org := range bind {
			if err := s.Bind(k, org, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("routing.Create: %v", err)
	}
	return NewRouting("default", &st)
}

func makeTeam(org, teamID string, repos []string, gitHooks bool) routing.Team {
	return routing.Team{
		OrganizationID: org, AuthURL: "https://api.example.com", TeamID: teamID,
		Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: repos, GitHooks: gitHooks,
			IncludePrompts: true, IncludeToolContent: true, Revision: 1,
			FetchedAt: time.Now().UTC(), TeamID: teamID, OrganizationID: org, AuthURL: "https://api.example.com",
		},
	}
}

func routedEnv(t *testing.T, stateDir, cwd, account string, rt *Routing) (Env, *spool.Spool, string) {
	t.Helper()
	spoolDir := filepath.Join(stateDir, spool.Dir)
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	sp.Bind(account)
	return Env{
		StateDir: stateDir, Cwd: cwd, Spool: sp, Account: account, Routing: rt, Now: time.Now().UTC(),
	}, sp, spoolDir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitx.Git(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func TestRoutedCrossHookAndCrossToolMixedAndStartRun(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	if err := os.WriteFile(filepath.Join(repoA, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoB, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA", "github.com/orgB/beta": "orgB"})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()

	sid := "018f3a2c-0001-7000-8000-000000000001"
	sub := "018f3a2c-0001-7000-8000-000000000002"
	env.StartRun(sid)
	run1 := env.runOf(sid)
	if run1 == "" {
		t.Fatal("StartRun did not record a run id")
	}

	r, err := env.Repo(ctx)
	if err != nil {
		t.Fatalf("Repo(repoA): %v", err)
	}
	sess := session.Session{ID: sid, Tool: "claude_code"}
	env.Touch(r, sess, "Edit", []string{filepath.Join(repoA, "a.go")}, map[string]any{semconv.TermaAgentIDKey: sub})

	c, ok := claim.Read(stateDir, sid, env.Time())
	if !ok || c.Kind != claim.KindRepository || c.ProjectID != "" || c.Run != run1 {
		t.Fatalf("claim after repoA touch = %+v (ok=%v); want KindRepository, empty ProjectID, run %q", c, ok, run1)
	}
	if subClaim, ok := claim.Read(stateDir, sub, env.Time()); !ok || subClaim.Kind != claim.KindRepository {
		t.Fatalf("subagent claim = %+v (ok=%v); want KindRepository", subClaim, ok)
	}

	// Hook 2: read into orgB repo via Classify -> marks run and subagent alias Mixed!
	env.Classify(ctx, r, sess, []string{filepath.Join(repoB, "b.go")}, nil)
	c, ok = claim.Read(stateDir, sid, env.Time())
	if !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after cross-org Classify = %+v (ok=%v); want KindMixed", c, ok)
	}
	if subClaim, ok := claim.Read(stateDir, sub, env.Time()); !ok || subClaim.Kind != claim.KindMixed {
		t.Fatalf("subagent claim after cross-org Classify = %+v (ok=%v); want KindMixed", subClaim, ok)
	}
	if p, _ := runs.Locate(stateDir, sid); p.MixedAt.IsZero() {
		t.Fatalf("run ledger not marked mixed: %+v", p)
	}

	// Fresh StartRun starts a clean run.
	env.StartRun(sid)
	run2 := env.runOf(sid)
	if run2 == "" || run2 == run1 {
		t.Fatalf("second StartRun = %q; want new id != %q", run2, run1)
	}
	env.Touch(r, sess, "Edit", []string{filepath.Join(repoA, "a.go")}, nil)
	c, ok = claim.Read(stateDir, sid, env.Time())
	if !ok || c.Kind != claim.KindRepository || c.Run != run2 {
		t.Fatalf("claim after fresh StartRun = %+v (ok=%v); want KindRepository with run %q", c, ok, run2)
	}
}

func TestRoutedUnknownReachAndWithholdPlacementsAndKeepRun(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA"})
	rt.WithholdPlacements = true
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()

	sid := "018f3a2c-0002-7000-8000-000000000001"
	env.StartRun(sid)
	r, err := env.Repo(ctx)
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	// UnknownReach marks ledger Mixed, but WithholdPlacements writes no claim file.
	env.Classify(ctx, r, session.Session{ID: sid, Tool: "claude_code"}, []string{UnknownReach + "eval\x00eval"}, nil)
	if p, _ := runs.Locate(stateDir, sid); p.MixedAt.IsZero() {
		t.Fatalf("UnknownReach did not mark ledger mixed: %+v", p)
	}
	if _, ok := claim.Read(stateDir, sid, env.Time()); ok {
		t.Fatal("WithholdPlacements still wrote a claim file")
	}

	// keepRun refreshes Active after runs.RefreshEvery.
	rt.WithholdPlacements = false
	sid2 := "018f3a2c-0002-7000-8000-000000000002"
	env.StartRun(sid2)
	before, _ := runs.Locate(stateDir, sid2)
	env.Now = env.Now.Add(runs.RefreshEvery + time.Minute)
	env.Touch(r, session.Session{ID: sid2, Tool: "claude_code"}, "Edit", []string{filepath.Join(repoA, "x.go")}, nil)
	after, _ := runs.Locate(stateDir, sid2)
	if !after.Active.After(before.Active) {
		t.Fatalf("keepRun did not advance Active: before=%v after=%v", before.Active, after.Active)
	}
}

func TestRoutedOwnerAccountConflictPreserved(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	fileA := filepath.Join(repoA, "a.go")
	if err := os.WriteFile(fileA, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rtA := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA"})
	envA, _, spoolDir := routedEnv(t, stateDir, repoA, testAccountA, rtA)
	ctx := context.Background()
	sid := "018f3a2c-0003-7000-8000-000000000001"
	rA, err := envA.Repo(ctx)
	if err != nil {
		t.Fatalf("Repo A: %v", err)
	}
	envA.Touch(rA, session.Session{ID: sid, Tool: "claude_code"}, "Edit", []string{fileA}, nil)

	// Switch to account B on the same session ID -> must NOT overwrite manifest and MUST count account_conflict.
	stB := routing.State{Profile: "default", Account: testAccountB, Teams: []routing.Team{
		func() routing.Team {
			tm := makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true)
			tm.EnrollmentID = "enr_b"
			return tm
		}(),
	}, Bindings: map[string]routing.Binding{"github.com/orgA/alpha": {Organization: "orgA"}}}
	envB, _, _ := routedEnv(t, stateDir, repoA, testAccountB, NewRouting("default", &stB))
	rB, err := envB.Repo(ctx)
	if err != nil {
		t.Fatalf("Repo B: %v", err)
	}
	envB.Touch(rB, session.Session{ID: sid, Tool: "claude_code"}, "Edit", []string{fileA}, nil)

	l := lossesIn(t, spoolDir)
	if l[sendas.LostAccountConflict] != 1 {
		t.Fatalf("losses = %v; want account_conflict=1", l)
	}
}

func TestRoutedSandboxedGitHooksAndNoDestinationLosses(t *testing.T) {
	stateDir := t.TempDir()
	repoOn := namedRepo(t, "on", "git@github.com:orgA/on.git")
	repoOff := namedRepo(t, "off", "git@github.com:orgB/off.git")
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/on"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/off"}, false),
	}, map[string]string{"github.com/orgA/on": "orgA", "github.com/orgB/off": "orgB"})

	if !rt.GitHooks(filepath.Join(repoOn, ".git"), time.Now()) {
		t.Fatal("GitHooks(repoOn) = false; want true")
	}
	if rt.GitHooks(filepath.Join(repoOff, ".git"), time.Now()) {
		t.Fatal("GitHooks(repoOff) = true; want false")
	}

	// PrepareCommitMsg stamps repoOn and skips repoOff.
	envOn, _, spoolDir := routedEnv(t, stateDir, repoOn, testAccountA, rt)
	ctx := context.Background()
	fOn := filepath.Join(repoOn, "work.txt")
	if err := os.WriteFile(fOn, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rOn, err := envOn.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sid := "018f3a2c-0004-7000-8000-000000000001"
	envOn.Touch(rOn, session.Session{ID: sid, Tool: "claude_code"}, "Edit", []string{fOn}, nil)
	gitRun(t, repoOn, "add", "work.txt")
	msgOn := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msgOn, []byte("feat: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPrep := envOn
	envPrep.Args = []string{msgOn, "message"}
	if err := PrepareCommitMsg(ctx, envPrep); err != nil {
		t.Fatal(err)
	}
	gotMsg, _ := os.ReadFile(msgOn)
	if !strings.Contains(string(gotMsg), "Agent-Session-Id: "+sid) {
		t.Fatalf("PrepareCommitMsg did not stamp repoOn:\n%s", gotMsg)
	}

	// Sandboxed git hook cannot read routing.State (Routing unusable, Spool nil) -> commits & parks with Enrollment == "".
	gitRun(t, repoOn, "commit", "-F", msgOn)
	sandboxEnv := Env{
		StateDir: filepath.Join(t.TempDir(), "blocked"),
		Cwd:      repoOn,
		Account:  sendas.AccountUnreadable,
		Routing:  NewRouting("default", nil), // unusable routing state
		Now:      time.Now().UTC(),
	}
	if err := PostCommit(ctx, sandboxEnv); err != nil {
		t.Fatalf("sandboxed PostCommit: %v", err)
	}

	// Also park a Hold event (Enrollment == "", HoldKey != "") in the same repo to prove unpark keeps Hold events.
	heldEv := spool.Event{
		Time: time.Now().UTC(), Name: semconv.TermaFilesTouchedEvent, SessionID: sid,
		Account: testAccountA, Profile: "default", HoldKey: "github.com/orgA/on",
	}
	if !envOn.park(rOn, parkedEvents, heldEv) {
		t.Fatal("failed to park held event")
	}

	// Unsandboxed Repo(ctx) runs unpark: drops destination-less commit (counts no_destination), keeps Hold event.
	if _, err := envOn.Repo(ctx); err != nil {
		t.Fatalf("unsandboxed Repo: %v", err)
	}
	l := lossesIn(t, spoolDir)
	if l[LostNoDestination] != 1 {
		t.Fatalf("losses = %v; want %s=1", l, LostNoDestination)
	}
	spooled := hookruntest.Spooled(t, envOn.Spool)
	hasHeld, hasCommit := false, false
	for _, ev := range spooled {
		if ev.HoldKey == "github.com/orgA/on" {
			hasHeld = true
		}
		if ev.Name == semconv.TermaCommitEvent {
			hasCommit = true
		}
	}
	if !hasHeld {
		t.Fatal("unpark dropped Hold event that had HoldKey set")
	}
	if hasCommit {
		t.Fatal("unpark spooled destination-less sandboxed commit instead of dropping it")
	}
}

func TestRoutedTurns(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	repoUn := namedRepo(t, "uncollected", "git@github.com:other/uncollected.git")
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA", "github.com/orgB/beta": "orgB"})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()

	turns := env.Turns(ctx)
	if !turns.Admits(repoA) || !turns.Admits(repoB) || turns.Admits(repoUn) {
		t.Fatalf("Turns.Admits = (%v, %v, %v); want (true, true, false)", turns.Admits(repoA), turns.Admits(repoB), turns.Admits(repoUn))
	}
	rA, _ := env.Repo(ctx)
	evA := turns.Routed(rA, repoA, spool.Event{SessionID: "sess-t", Attrs: map[string]any{AttrProjectID: "old"}})
	evB := turns.Routed(rA, repoB, spool.Event{SessionID: "sess-t", Attrs: map[string]any{AttrProjectID: "old"}})
	if evA.Enrollment == "" || evB.Enrollment == "" || evA.Enrollment == evB.Enrollment || !evB.OwnLocation {
		t.Fatalf("per-turn routing failed: evA=%+v evB=%+v", evA, evB)
	}
	if _, hasProj := evB.Attrs[AttrProjectID]; hasProj {
		t.Fatalf("routed event retained %s attr", AttrProjectID)
	}
}

func TestRoutedHookLatencyBudget(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	fileA := filepath.Join(repoA, "a.go")
	if err := os.WriteFile(fileA, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA"})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()
	sid := "018f3a2c-0005-7000-8000-000000000001"
	env.StartRun(sid)
	r, err := env.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.Session{ID: sid, Tool: "claude_code"}
	env.Touch(r, sess, "Edit", []string{fileA}, nil)
	gitRun(t, repoA, "add", "a.go")

	const budget = 50 * time.Millisecond
	measure := func(name string, fn func()) {
		t.Helper()
		fn() // warmup
		durs := make([]time.Duration, 7)
		for i := range durs {
			start := time.Now()
			fn()
			durs[i] = time.Since(start)
		}
		slices.Sort(durs)
		if med := durs[len(durs)/2]; med > budget {
			t.Errorf("%s median latency = %v; exceeds %v budget (samples=%v)", name, med, budget, durs)
		}
	}

	measure("Read Classify", func() {
		env.Classify(ctx, nil, sess, []string{fileA}, nil)
	})
	measure("Bash Classify", func() {
		env.Classify(ctx, nil, sess, Alone(ShellPaths("cat a.go | grep package", repoA)...), nil)
	})
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	measure("PrepareCommitMsg", func() {
		if err := os.WriteFile(msgPath, []byte("feat: perf\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e := env
		e.Args = []string{msgPath, "message"}
		if err := PrepareCommitMsg(ctx, e); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRoutedOutsideGitPersonalAndSymlinkEditMixed(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	// A folder of the developer's home: a temp folder that is not is no one's (nobodys).
	outsideDir := t.TempDir()
	t.Setenv("HOME", outsideDir)
	t.Setenv("USERPROFILE", outsideDir)
	personalFile := filepath.Join(outsideDir, "diary.txt")
	if err := os.WriteFile(personalFile, []byte("private\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(targetB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkInA := filepath.Join(repoA, "link.go")
	if err := os.Symlink(targetB, linkInA); err != nil {
		t.Fatal(err)
	}

	personal := makeTeam("orgPersonal", "team-personal", nil, false)
	personal.Personal = true
	personal.Policy.Mode = config.ModeGlobal
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
		personal,
	}, map[string]string{"github.com/orgA/alpha": "orgA", "github.com/orgB/beta": "orgB"})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()
	rA, err := env.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Reading a file outside git while personal team is selected marks orgA session Mixed.
	sid1 := "018f3a2c-0006-7000-8000-000000000001"
	env.StartRun(sid1)
	env.Classify(ctx, rA, session.Session{ID: sid1, Tool: "claude_code"}, []string{personalFile}, nil)
	if c, ok := claim.Read(stateDir, sid1, env.Time()); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after reading outside-git file with personal team = %+v (ok=%v); want KindMixed", c, ok)
	}

	// 2. Editing a symlink in orgA pointing into orgB marks orgA session Mixed.
	sid2 := "018f3a2c-0006-7000-8000-000000000002"
	env.StartRun(sid2)
	env.Touch(rA, session.Session{ID: sid2, Tool: "claude_code"}, "Edit", []string{linkInA}, nil)
	if c, ok := claim.Read(stateDir, sid2, env.Time()); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after editing symlink into orgB = %+v (ok=%v); want KindMixed", c, ok)
	}
}

func TestRoutedGitCommitDoesNotTaintActiveRunInOtherOrg(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	fA := filepath.Join(repoA, "a.txt")
	if err := os.WriteFile(fA, []byte("from A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{"github.com/orgA/alpha": "orgA", "github.com/orgB/beta": "orgB"})
	ctx := context.Background()
	sid := "018f3a2c-0007-7000-8000-000000000001"

	// Run 1: touch file in repoA and stage it.
	envA, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	envA.StartRun(sid)
	rA, err := envA.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	envA.Touch(rA, session.Session{ID: sid, Tool: "claude_code"}, "Edit", []string{fA}, nil)
	gitRun(t, repoA, "add", "a.txt")

	// Run 2 (fresh hook process): session resumes cleanly in repoB (orgB).
	envB, _, _ := routedEnv(t, stateDir, repoB, testAccountA, LoadRouting(stateDir, "default"))
	envB.StartRun(sid)
	rB, err := envB.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	envB.EmitStart(rB, session.Session{ID: sid, Tool: "claude_code"}, nil)
	before, ok := claim.Read(stateDir, sid, envB.Time())
	if !ok || before.Kind != claim.KindRepository || before.Repository.Origin != "github.com/orgB/beta" {
		t.Fatalf("claim before commit in A = %+v (ok=%v); want repoB", before, ok)
	}

	// Git commit hooks run in repoA stamping sid: must NOT place or mark repoB's active run Mixed.
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msgPath, []byte("feat: commit A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepEnv, _, _ := routedEnv(t, stateDir, repoA, testAccountA, LoadRouting(stateDir, "default"))
	prepEnv.Args = []string{msgPath, "message"}
	if err := PrepareCommitMsg(ctx, prepEnv); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoA, "commit", "-F", msgPath)
	postEnv, _, _ := routedEnv(t, stateDir, repoA, testAccountA, LoadRouting(stateDir, "default"))
	if err := PostCommit(ctx, postEnv); err != nil {
		t.Fatal(err)
	}
	after, ok := claim.Read(stateDir, sid, postEnv.Time())
	if !ok || after.Kind != claim.KindRepository || after.Repository.Origin != "github.com/orgB/beta" || after.Run != before.Run {
		t.Fatalf("git hooks in repoA tainted repoB run: after=%+v (ok=%v), want repoB run %q", after, ok, before.Run)
	}
}

func TestSpoolTitleWithholdsAfterRouteChange(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	teamA := makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true)
	teamB := makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true)
	rt := routedState(t, stateDir, testAccountA, []routing.Team{teamA, teamB}, map[string]string{
		"github.com/orgA/alpha": "orgA",
		"github.com/orgB/beta":  "orgB",
	})
	ctx := context.Background()
	sid := "018f3a2c-0009-7000-8000-000000000001"
	envA, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	rA, err := envA.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	SpoolTitle(envA, rA, "titles", sid, map[string]any{}, func() (string, bool, error) { return "Title A", true, nil })
	envB, _, _ := routedEnv(t, stateDir, repoB, testAccountA, LoadRouting(stateDir, "default"))
	rB, err := envB.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	SpoolTitle(envB, rB, "titles", sid, map[string]any{}, func() (string, bool, error) { return "Title B", true, nil })
	SpoolTitle(envA, rA, "titles", sid, map[string]any{}, func() (string, bool, error) { return "Title A2", true, nil })
	got := hookruntest.Named(hookruntest.Spooled(t, envA.Spool), semconv.TermaSessionTitleEvent)
	if len(got) != 1 || got[0].Attrs[semconv.TermaSessionTitleKey] != "Title A" || got[0].Enrollment != rA.dest.d.EnrollmentID {
		t.Fatalf("titles = %+v; want only initial Title A for orgA", got)
	}
}

func TestUnparkMixedEventsAndPushesPreserveIdempotency(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	teamA := makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true)
	rt := routedState(t, stateDir, testAccountA, []routing.Team{teamA}, map[string]string{"github.com/orgA/alpha": "orgA"})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Mixed parked events: block spool/losses so CountLost fails after AppendAsCollected commits.
	lossesDir := filepath.Join(stateDir, spool.Dir, "losses")
	if err := os.WriteFile(lossesDir, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.park(r, parkedEvents, spool.Event{Time: env.Time(), Name: "terma.git.commit", Enrollment: r.dest.d.EnrollmentID, Account: testAccountA})
	env.park(r, parkedEvents, spool.Event{Time: env.Time(), Name: "terma.git.commit", Account: testAccountA})
	env.unpark(r)
	if err := os.Remove(lossesDir); err != nil {
		t.Fatal(err)
	}
	env.unpark(r)
	if got := hookruntest.Named(hookruntest.Spooled(t, env.Spool), "terma.git.commit"); len(got) != 1 {
		t.Fatalf("spooled commits after mixed unpark retry = %d, want 1 (no duplicate replay)", len(got))
	}
	readNoDest := func() int {
		l, err := spool.ReadLosses(filepath.Join(stateDir, spool.Dir), env.Time())
		if err != nil {
			t.Fatal(err)
		}
		return l.Counts[LostNoDestination]
	}
	if lost := readNoDest(); lost != 1 {
		t.Fatalf("no_destination lost after parked-loss import = %d, want 1", lost)
	}

	// Mixed parked pushes: block pushes dir so WriteJSON fails; CountLost must NOT run until writes commit.
	pushesPath := filepath.Join(stateDir, PushesDir)
	if err := os.WriteFile(pushesPath, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	var awaited []string
	env.AwaitPush = func(p string) { awaited = append(awaited, p) }
	env.park(r, parkedPushes, pushRecord{ID: "p-valid", Root: r.Root, GitDir: r.GitDir, Event: spool.Event{Enrollment: r.dest.d.EnrollmentID}, Refs: []PushRef{{LocalRef: "refs/heads/main"}}})
	env.park(r, parkedPushes, pushRecord{ID: "p-nodest", Root: r.Root, GitDir: r.GitDir, Refs: []PushRef{{LocalRef: "refs/heads/main"}}})
	env.unparkPushes(r)
	if lost := readNoDest(); lost != 1 {
		t.Fatalf("no_destination inflated while push WriteJSON failed: got %d, want 1", lost)
	}
	if err := os.Remove(pushesPath); err != nil {
		t.Fatal(err)
	}
	env.unparkPushes(r)
	if len(awaited) != 1 {
		t.Fatalf("awaited pushes = %v, want 1", awaited)
	}
	if lost := readNoDest(); lost != 2 {
		t.Fatalf("no_destination after push retry = %d, want 2", lost)
	}
}
