package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// mixed reports whether sessionID's current run is Mixed, in the ledger and in its claim.
func (m *routedMachine) mixed(sessionID string) (ledger, placement bool) {
	_, ledger = runs.MixedAt(m.stateDir, sessionID, time.Time{})
	c, ok := claim.Read(m.stateDir, sessionID, time.Now())
	return ledger, ok && c.Kind == claim.KindMixed
}

// The plan's scenario: a session started in github.com/dawson/side, personal's, edits
// github.com/acme/api. From that edit's hook the run is Mixed, in the run ledger and in a
// mixed placement, while the edit's file event goes to Acme Platform by its own checkout.
func TestATouchOfAnotherOrganizationsCheckoutMakesTheRunMixed(t *testing.T) {
	t.Parallel()
	st := acmeAndPersonal(t)
	m := newRoutedMachine(t, st)
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "crossing-session"
	r := startIn(t, m.hook(), sid, side)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatal("Mixed before any crossing")
	}
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("after the crossing: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	touches := hookruntest.Named(m.spooled(), semconv.TermaFilesTouchedEvent)
	if len(touches) != 1 || touches[0].Enrollment != hookruntest.Enrollment(t, st, "acme-platform") || !touches[0].OwnLocation ||
		touches[0].Repository.Origin != "github.com/acme/api" {
		t.Fatalf("file events %+v, want one to Acme Platform by its own checkout", touches)
	}
}

// Every touched checkout is classified before anything filters it: one under a binding
// still to decide crosses though nothing collects it yet, and one nothing collects and
// nothing binds belongs to no organization and does not.
func TestEveryTouchedCheckoutIsClassifiedBeforeFiltering(t *testing.T) {
	t.Parallel()
	// ClientCo lists github.com/torvalds/linux, which nobody has decided; personal collects
	// only github.com/dawson/*, so github.com/other/lib is nobody's.
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("clientco", "clientco-contractors", true, "github.com/torvalds/linux"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, nil)
	for name, tc := range map[string]struct {
		origin string
		mixed  bool
	}{
		"pending":  {"github.com/torvalds/linux", true},
		"nobody's": {"github.com/other/lib", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t, st)
			side, other := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, tc.origin)
			r := startIn(t, m.hook(), "touching-session", side)
			m.hook().Touch(r, sessionOf("touching-session"), "Read", []string{filepath.Join(other, "README")}, nil)
			if ledger, placement := m.mixed("touching-session"); ledger != tc.mixed || placement != tc.mixed {
				t.Errorf("Mixed in the ledger %v, in the placement %v; want %v", ledger, placement, tc.mixed)
			}
			for _, ev := range hookruntest.Named(m.spooled(), semconv.TermaFilesTouchedEvent) {
				if ev.Repository.Origin == "github.com/other/lib" {
					t.Errorf("spooled a file event for a checkout nothing collects: %+v", ev)
				}
			}
		})
	}
}

// A subagent's crossing is its parent's: the session's run turns Mixed, and so does the
// subagent's own placement.
func TestASubagentsCrossingMakesItsParentsRunMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid, agent = "parent-session", "agent-9"
	r := startIn(t, m.hook(), sid, side)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, AgentAttrs(map[string]any{}, agent, "general-purpose"))
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("the parent's run: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if c := m.claim(agent); c.Kind != claim.KindMixed {
		t.Errorf("the subagent's placement %+v", c)
	}
	if _, mixed := runs.MixedAt(m.stateDir, agent, time.Now()); !mixed {
		t.Error("the subagent's ID does not read its session's Mixed run")
	}
}

// The run's own placement moving counts as touching: a company run whose directory moves
// outside git, where personal collects, has crossed.
func TestACompanyRunMovingOutsideGitCrosses(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	api := hookruntest.RepoAt(t, "github.com/acme/api")
	scratch, _ := filepath.EvalSymlinks(t.TempDir())
	const sid = "moving-session"
	startIn(t, m.hook(), sid, api)
	ClaimFromPayload(context.Background(), m.hook(), PayloadSession{ID: sid, Cwd: scratch}, "claude-code")
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
}

// Mixed only turns on: a later hook of the run, back in its first repository, leaves the
// mixed placement as it is. Only a new run, a resume, ends it.
func TestOnlyANewRunEndsMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid = "resumed-session"
	r := startIn(t, m.hook(), sid, side)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	env := m.hook()
	r, _ = env.Open(context.Background(), sid, side)
	env.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: sid, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}})
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("a later hook of the run turned Mixed off: ledger %v, placement %v", ledger, placement)
	}
	startIn(t, m.hook(), sid, side)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatalf("the resumed run is still Mixed: ledger %v, placement %v", ledger, placement)
	}
	if c := m.claim(sid); c.Kind != claim.KindRepository || c.Repository.Origin != "github.com/dawson/side" {
		t.Errorf("the new run's placement %+v", c)
	}
}

// A crossing seen by one subagent's hook covers every identity the run's telemetry uses:
// the session's ID, that subagent's, and every other subagent the run ledger names, whose
// placements turn mixed too.
func TestAMixedRunCoversEverySubagent(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side, api := hookruntest.RepoAt(t, "github.com/dawson/side"), hookruntest.RepoAt(t, "github.com/acme/api")
	const sid, quiet, crossing = "fanned-session", "agent-quiet", "agent-crossing"
	r := startIn(t, m.hook(), sid, side)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(side, "a.go")}, AgentAttrs(map[string]any{}, quiet, "general-purpose"))
	if c := m.claim(quiet); c.Kind != claim.KindRepository {
		t.Fatalf("the quiet subagent's placement before the crossing: %+v", c)
	}
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, AgentAttrs(map[string]any{}, crossing, "general-purpose"))
	for _, id := range []string{sid, crossing, quiet} {
		if c := m.claim(id); c.Kind != claim.KindMixed || c.Run != m.claim(sid).Run {
			t.Errorf("%s's placement after the crossing: %+v", id, c)
		}
	}
}

// The run a session's events name comes from the run ledger, so a session idle for longer
// than its claim lasts still names the run it is in, not none.
func TestARunOutlivesItsClaim(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	const sid = "idle-session"
	startIn(t, m.hook(), sid, side)
	run := hookruntest.Named(m.spooled(), semconv.TermaSessionStartEvent)[0].Run
	later := m.hook()
	later.Now = time.Now().Add(5 * time.Hour)
	if _, ok := claim.Read(m.stateDir, sid, later.Now); ok {
		t.Fatal("the claim has not expired yet")
	}
	r, _ := later.Open(context.Background(), sid, side)
	later.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: sid, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}})
	calls := hookruntest.Named(m.spooled(), semconv.TermaToolCallEvent)
	if run == "" || len(calls) != 1 || calls[0].Run != run {
		t.Fatalf("after the claim expired the event names run %q, want %q", calls[0].Run, run)
	}
}

// A run remembers in the run ledger where it has been, so two hooks that each see half a
// crossing make it Mixed between them. The first touches Acme's checkout from one that
// belongs to no organization, which crosses nothing; the second places the run in personal's
// checkout. The first hook reports from a checkout Route drops, as classifyTouches sees it.
func TestACrossingSeenAcrossTwoHooksMakesTheRunMixed(t *testing.T) {
	t.Parallel()
	// Personal collects only github.com/dawson/*, so github.com/other/lib is nobody's.
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, map[string]string{"github.com/acme": "acme"})
	m := newRoutedMachine(t, st)
	lib, api := hookruntest.RepoAt(t, "github.com/other/lib"), hookruntest.RepoAt(t, "github.com/acme/api")
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	const sid = "two-hook-session"
	first := m.hook()
	first.StartRun(sid)
	first.Cwd = lib
	r, err := first.routeHere(context.Background())
	if err != nil || r.Collected() {
		t.Fatalf("github.com/other/lib: %v, collected %v", err, err == nil && r.Collected())
	}
	first.Report(r, sessionOf(sid), "Read", []string{filepath.Join(api, "README")}, nil)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatal("Mixed after touching Acme's checkout from one that belongs to no organization")
	}
	ClaimFromPayload(context.Background(), m.hook(), PayloadSession{ID: sid, Cwd: side}, "claude-code")
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("placed in personal's checkout after touching Acme's: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
}

// The run ledger outlives the claim: a run that touched Acme's checkout, then idle for
// longer than its claim lasts, crosses when it is next placed in personal's checkout.
func TestACrossingIsSeenPastTheClaim(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	api, side := hookruntest.RepoAt(t, "github.com/acme/api"), hookruntest.RepoAt(t, "github.com/dawson/side")
	const sid = "idle-crossing-session"
	r := startIn(t, m.hook(), sid, api)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	later := m.hook()
	later.Now = time.Now().Add(5 * time.Hour)
	if _, ok := claim.Read(m.stateDir, sid, later.Now); ok {
		t.Fatal("the claim has not expired yet")
	}
	ClaimFromPayload(context.Background(), later, PayloadSession{ID: sid, Cwd: side}, "claude-code")
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
}

// A resume begins a run that has been nowhere: placed in personal's checkout after the
// previous run touched Acme's, it is not Mixed.
func TestAResumedRunRemembersNoLocations(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	api, side := hookruntest.RepoAt(t, "github.com/acme/api"), hookruntest.RepoAt(t, "github.com/dawson/side")
	const sid = "resumed-elsewhere"
	r := startIn(t, m.hook(), sid, api)
	m.hook().Touch(r, sessionOf(sid), "Edit", []string{filepath.Join(api, "main.go")}, nil)
	startIn(t, m.hook(), sid, side)
	ClaimFromPayload(context.Background(), m.hook(), PayloadSession{ID: sid, Cwd: side}, "claude-code")
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatalf("the resumed run: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); !slices.Equal(locs, []string{"github.com/dawson/side"}) {
		t.Errorf("the resumed run has been to %v", locs)
	}
}

// Worktrees of one pending repository never cross each other, from hook to hook either: a
// run placed in one, then past its claim in another, is not Mixed.
func TestWorktreesOfOnePendingRepositoryDoNotCrossAcrossHooks(t *testing.T) {
	t.Parallel()
	// ClientCo lists github.com/torvalds/linux, which nobody has decided.
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("clientco", "clientco-contractors", true, "github.com/torvalds/linux"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, nil)
	m := newRoutedMachine(t, st)
	main := hookruntest.RepoAt(t, "github.com/torvalds/linux")
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "linux-fix")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	wt, _ = filepath.EvalSymlinks(wt)
	const sid = "pending-worktrees"
	startIn(t, m.hook(), sid, main)
	later := m.hook()
	later.Now = time.Now().Add(5 * time.Hour)
	ClaimFromPayload(ctx, later, PayloadSession{ID: sid, Cwd: wt}, "claude-code")
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); !slices.Equal(locs, []string{"github.com/torvalds/linux"}) {
		t.Errorf("the run has been to %v", locs)
	}
}

// A hook writes the run ledger only when it sees the run somewhere new.
func TestTheLedgerIsWrittenOnlyForANewLocation(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	api, web := hookruntest.RepoAt(t, "github.com/acme/api"), hookruntest.RepoAt(t, "github.com/acme/web")
	const sid = "quiet-ledger"
	startIn(t, m.hook(), sid, api)
	path := filepath.Join(m.stateDir, runs.Dir, sid+".json")
	stat := func() os.FileInfo {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	touch := func(file string) {
		t.Helper()
		env := m.hook()
		r, ok := env.Open(context.Background(), sid, api)
		if !ok {
			t.Fatal("no checkout")
		}
		env.Touch(r, sessionOf(sid), "Edit", []string{file}, nil)
	}
	before := stat()
	touch(filepath.Join(api, "main.go"))
	if !os.SameFile(before, stat()) {
		t.Fatal("a hook with nothing new rewrote the ledger")
	}
	touch(filepath.Join(web, "main.go"))
	if os.SameFile(before, stat()) {
		t.Fatal("a hook that saw a new location left the ledger as it was")
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); !slices.Equal(locs, []string{"github.com/acme/api", "github.com/acme/web"}) {
		t.Errorf("the run has been to %v", locs)
	}
}

// The run ledger keeps at most maxRunLocations locations for a run. A run that has been to
// more, all Acme's here, turns Mixed: the ledger could no longer rule a crossing out.
func TestARunPastTheLocationBoundTurnsMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	const sid = "wandering-session"
	env := m.hook()
	env.StartRun(sid)
	at := func(i int) routing.Location {
		return routing.Location{Kind: routing.Repository, Repository: fmt.Sprintf("github.com/acme/r%d", i)}
	}
	var touched []routing.Location
	for i := 1; i < maxRunLocations; i++ {
		touched = append(touched, at(i))
	}
	c := claim.Claim{Kind: claim.KindRepository, Tool: "claude-code"}
	env.place(sid, "", c, at(0), touched)
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatalf("Mixed at %d locations, all Acme's", maxRunLocations)
	}
	next := m.hook()
	next.place(sid, "", c, at(0), []routing.Location{at(maxRunLocations)})
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("past the bound: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); len(locs) != maxRunLocations {
		t.Errorf("the ledger keeps %d locations", len(locs))
	}
}

// acmeAndPersonalRepos is a machine where Acme Platform collects github.com/acme/*, bound to
// Acme, and personal only github.com/dawson/*: github.com/other/lib, and every folder
// outside git, is nobody's.
func acmeAndPersonalRepos(t *testing.T) *routing.State {
	return hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, map[string]string{"github.com/acme": "acme"})
}

// Two hooks of one run, a session's and its subagent's, each in a checkout nobody collects,
// each see half a crossing: one reaches Acme's checkout, the other personal's. The second
// read the run ledger before the first wrote to it, so neither read the other's location;
// the second, writing after the first, is told it by the ledger, and the run turns Mixed.
func TestTwoInterleavedHooksThatEachSeeHalfACrossingMakeTheRunMixed(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonalRepos(t))
	repo := func(origin string) routing.Location {
		return routing.Location{Kind: routing.Repository, Repository: origin}
	}
	lib := repo("github.com/other/lib")
	const sid = "interleaved-session"
	first, second := m.hook(), m.hook()
	first.StartRun(sid)
	if cur := second.currentRun(sid); cur.id == "" || len(cur.locations) != 0 {
		t.Fatalf("the second hook read run %q at %v", cur.id, cur.locations)
	}
	c := claim.Claim{Kind: claim.KindRepository, Tool: "claude-code", Repository: config.Repository{Origin: lib.Repository}}
	first.place(sid, "", c, lib, []routing.Location{repo("github.com/acme/api")})
	if ledger, placement := m.mixed(sid); ledger || placement {
		t.Fatal("Mixed after half a crossing")
	}
	second.place(sid, "agent-7", c, lib, []routing.Location{repo("github.com/dawson/side")})
	if ledger, placement := m.mixed(sid); !ledger || !placement {
		t.Fatalf("after both halves: ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	if c := m.claim("agent-7"); c.Kind != claim.KindMixed {
		t.Errorf("the subagent's placement %+v", c)
	}
}

// The ledger write, on its own: a crossing the run ledger refuses, its lock held past the
// hook's wait, is recorded in its sidecar, which every reader takes for Mixed from the
// crossing, and the hook waits on the lock once, not again for the locations it saw. The
// run's next hook that can lock the ledger folds the sidecar in and records them.
func TestARefusedCrossingIsRecordedAside(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t, acmeAndPersonal(t))
	const sid = "refused-session"
	env := m.hook()
	env.StartRun(sid)
	unlock, err := flock.TryLock(filepath.Join(m.stateDir, runs.Dir, sid+".json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	at := env.Time()
	cur := env.currentRun(sid)
	v := verdict{seen: []string{"github.com/acme/api"}, mixed: true, since: at, crossedWith: func([]string) bool { return true }}
	began := time.Now()
	if !env.recordRun(sid, cur, v, at) {
		t.Fatal("recordRun did not report the run Mixed")
	}
	if waited := time.Since(began); waited > runsLockWait+time.Second {
		t.Fatalf("recordRun waited %v: on the lock more than once", waited)
	}
	unlock()
	if from, mixed := runs.MixedAt(m.stateDir, sid, time.Time{}); !mixed || !from.Equal(at.UTC()) {
		t.Fatalf("the run reads Mixed %v from %v, want from %v", mixed, from, at.UTC())
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); len(locs) != 0 {
		t.Fatalf("the ledger recorded %v through a held lock", locs)
	}
	sidecar := filepath.Join(m.stateDir, runs.Dir, sid+".mixed")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("no sidecar: %v", err)
	}

	next := m.hook()
	cur = next.currentRun(sid)
	if !cur.aside || cur.mixedAt.IsZero() {
		t.Fatalf("the next hook reads %+v", cur)
	}
	now := next.Time()
	if !next.recordRun(sid, cur, next.decide(sid, cur, []routing.Location{{Kind: routing.Repository, Repository: "github.com/acme/api"}}, now), now) {
		t.Fatal("the next hook's run is not Mixed")
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("the next hook left the sidecar: %v", err)
	}
	if _, locs, _ := runs.Locations(m.stateDir, sid); !slices.Equal(locs, []string{"github.com/acme/api"}) {
		t.Errorf("the next hook recorded %v", locs)
	}
	if from, mixed := runs.MixedAt(m.stateDir, sid, time.Time{}); !mixed || !from.Equal(at.UTC()) {
		t.Errorf("after the fold the run reads Mixed %v from %v, want from %v", mixed, from, at.UTC())
	}
}

// runsLockWait is how long a ledger write waits for its lock (runs.lockWait).
const runsLockWait = 2 * time.Second
