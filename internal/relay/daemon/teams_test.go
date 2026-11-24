package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/policy"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	testAuthURL = "https://auth.example"
	testOTLPURL = "https://otlp.example"
	testOrgA    = "org-a"
	testOrgB    = "org-b"
)

func testAccount(user string) string { return auth.AccountKey(testAuthURL, user) }

func activeTeam(enrollment, teamID, orgID string, personal bool, now time.Time, repos ...string) routing.Team {
	pol := config.Policy{
		Mode:           config.ModeRepo,
		TeamID:         teamID,
		OrganizationID: orgID,
		AuthURL:        testAuthURL,
		Revision:       1,
		FetchedAt:      now,
	}
	pol.Repositories = append(pol.Repositories, repos...)
	if personal {
		pol.Mode = config.ModeGlobal
		pol.DefaultProjectID = teamID
	}
	return routing.Team{
		EnrollmentID:   enrollment,
		TeamID:         teamID,
		OrganizationID: orgID,
		AuthURL:        testAuthURL,
		Personal:       personal,
		Epoch:          1,
		Policy:         pol,
	}
}

func writeRoutingState(t *testing.T, stateDir string, st routing.State) routing.State {
	t.Helper()
	saved, err := routing.Replace(stateDir, st.Profile, nil, st.Account, func(s *routing.State) error {
		s.Teams = st.Teams
		s.Bindings = st.Bindings
		return nil
	})
	if err != nil {
		t.Fatalf("routing.Replace: %v", err)
	}
	return saved
}

func setupProfileDir(t *testing.T, dir, profile string, harnesses []string) {
	t.Helper()
	if err := config.UpdateProfile(dir, profile, func(p *config.Profile) {
		p.Harnesses = harnesses
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
}

// Run writes RunInfo.Routing = true in relay/run.json and RunningRelay reads it back.
func TestRunInfoRecordsRouting(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := runConfig(stateDir, time.Hour, nil)
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, c); done <- err }()

	for {
		if _, err := os.Stat(filepath.Join(dir, RunFile)); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("relay exited before writing RunFile: %v", err)
		case <-ctx.Done():
			t.Fatal("relay never wrote RunFile")
		case <-time.After(10 * time.Millisecond):
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, RunFile))
	if err != nil {
		t.Fatal(err)
	}
	var fromDisk RunInfo
	if err := json.Unmarshal(raw, &fromDisk); err != nil || !fromDisk.Routing {
		t.Fatalf("run.json = %s, parsed %+v, err %v; want Routing=true", raw, fromDisk, err)
	}
	info, ok := RunningRelay(dir)
	if !ok || !info.Routing {
		t.Fatalf("RunningRelay = %+v, %v; want ok=true, Routing=true", info, ok)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TeamRefresher skips and fails closed on account mismatch, uses PolicyRefreshInterval (1m)
// for Active enrollments and RefusedRecheck (24h) for Refused enrollments, classifies
// ErrOtherEnvironment as Transient, commits strictly through CAS routing.Update, writes
// StoreTeamPolicy only after commit, and applies OrgAccessLost across matching-epoch
// siblings while sparing newer-epoch siblings and other orgs.
func TestTeamRefresherAccountIntervalsCASAndOrgAccessLost(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfgDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice := testAccount("alice")
	bob := testAccount("bob")

	t1 := activeTeam("e1", "team-1", testOrgA, false, now.Add(-2*time.Minute), "github.com/a/r1")
	t2 := activeTeam("e2", "team-2", testOrgA, false, now.Add(-2*time.Minute), "github.com/a/r2")
	t3 := activeTeam("e3", "team-3", testOrgB, false, now.Add(-2*time.Minute), "github.com/b/r3")
	tRef := activeTeam("e-ref", "team-ref", testOrgB, false, now.Add(-time.Hour), "github.com/b/ref")
	tRef.Refusals = 2
	tRef.RefusedAt = now.Add(-3 * time.Hour)

	writeRoutingState(t, stateDir, routing.State{
		Profile:    "default",
		Account:    alice,
		Generation: 1,
		Teams:      []routing.Team{t1, t2, t3, tRef},
	})

	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, Account: bob}
	var fetched atomic.Int32
	var changed []string
	var changeMu sync.Mutex
	var fetchFn func(context.Context, *config.Config, string, routing.Team) (routing.Refresh, error)

	d := Deps{
		LoadConfig: func() (*config.Config, error) { return cfg, nil },
		RefreshTeam: func(ctx context.Context, c *config.Config, acct string, tm routing.Team) (routing.Refresh, error) {
			fetched.Add(1)
			return fetchFn(ctx, c, acct, tm)
		},
	}
	ref := d.TeamRefresher(func(ids ...string) {
		changeMu.Lock()
		changed = append(changed, ids...)
		changeMu.Unlock()
	})

	// 1. Account mismatch: fails closed, lists nothing, refreshes nothing.
	if got := ref.Teams(); len(got) != 0 {
		t.Fatalf("Teams on account mismatch = %v, want empty", got)
	}
	if err := ref.Refresh(t.Context(), "e1"); err != nil || fetched.Load() != 0 {
		t.Fatalf("Refresh on account mismatch called fetch (%d, %v)", fetched.Load(), err)
	}

	// 2. Matching account: intervals 1m (Active) vs 24h (Refused) and Fetched timestamps.
	cfg.Account = alice
	if got := ref.Teams(); !slices.Equal(got, []string{"e1", "e2", "e3", "e-ref"}) {
		t.Fatalf("Teams = %v", got)
	}
	if got := ref.Every("e1"); got != PolicyRefreshInterval {
		t.Fatalf("Every(active) = %v, want %v", got, PolicyRefreshInterval)
	}
	if got := ref.Every("e-ref"); got != RefusedRecheck {
		t.Fatalf("Every(refused) = %v, want %v", got, RefusedRecheck)
	}
	if !ref.Fetched("e-ref").Equal(tRef.RefusedAt) {
		t.Fatalf("Fetched(refused) = %v, want %v", ref.Fetched("e-ref"), tRef.RefusedAt)
	}

	// 3. ErrOtherEnvironment -> Transient: preserves policy and does not advance epoch or fire onChange.
	fetchFn = func(_ context.Context, _ *config.Config, _ string, tm routing.Team) (routing.Refresh, error) {
		return routing.Refresh{
			EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch,
			Failure: routing.Transient, At: now,
		}, policy.ErrOtherEnvironment
	}
	if err := ref.Refresh(t.Context(), "e1"); !errors.Is(err, policy.ErrOtherEnvironment) {
		t.Fatalf("other-env err = %v, want ErrOtherEnvironment", err)
	}
	st, _, _ := routing.Load(stateDir, "default")
	got1, _ := st.Team("e1")
	if got1.LastError != routing.Transient || got1.Epoch != 1 || got1.StateAt(now) != routing.Active {
		t.Fatalf("after other-env transient: %+v", got1)
	}
	if len(changed) != 0 {
		t.Fatalf("transient error unexpectedly fired onChange: %v", changed)
	}

	// 4. Stale-epoch CAS conflict: concurrent update bumps e1's epoch before refresh commits;
	// StoreTeamPolicy MUST NOT overwrite policies/<team>.json and onChange MUST NOT fire.
	stalePol := got1.Policy
	stalePol.Revision = 99
	stalePol.FetchedAt = now.Add(time.Minute)
	fetchFn = func(_ context.Context, _ *config.Config, _ string, tm routing.Team) (routing.Refresh, error) {
		// Simulate a concurrent CAS writer advancing e1's epoch while fetch is in flight.
		_, uerr := routing.Update(stateDir, "default", routing.Expect{Account: alice}, func(s *routing.State) error {
			p := tm.Policy
			p.Revision = 2
			p.FetchedAt = now
			_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &p, At: now})
			return err
		})
		if uerr != nil {
			t.Fatalf("concurrent update: %v", uerr)
		}
		return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &stalePol, At: now}, nil
	}
	if err := ref.Refresh(t.Context(), "e1"); err != nil {
		t.Fatalf("stale-epoch refresh err: %v", err)
	}
	if _, ok, _ := config.ReadPolicy(stateDir, "team-1"); ok {
		t.Fatal("StoreTeamPolicy wrote policy file despite stale-epoch CAS rejection")
	}
	if len(changed) != 0 {
		t.Fatalf("stale-epoch refresh fired onChange: %v", changed)
	}

	// 5. Clean commit writes StoreTeamPolicy strictly after commit and fires onChange.
	fetchFn = func(_ context.Context, _ *config.Config, _ string, tm routing.Team) (routing.Refresh, error) {
		p := tm.Policy
		p.Revision = 3
		p.FetchedAt = now.Add(2 * time.Minute)
		return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &p, At: now}, nil
	}
	if err := ref.Refresh(t.Context(), "e1"); err != nil {
		t.Fatalf("clean refresh err: %v", err)
	}
	stored, ok, err := config.ReadPolicy(stateDir, "team-1")
	if err != nil || !ok || stored.Revision != 3 {
		t.Fatalf("stored policy = %+v, ok=%v, err=%v; want Revision=3", stored, ok, err)
	}
	if !slices.Equal(changed, []string{"e1"}) {
		t.Fatalf("onChange = %v, want [e1]", changed)
	}
	changed = nil

	// 6. OrgAccessLost with pre-request per-enrollment epoch snapshot:
	// e2 advances its epoch during e1's in-flight request -> e1 is refused, e2 is spared,
	// e3 (other org) is untouched.
	fetchFn = func(_ context.Context, _ *config.Config, _ string, tm routing.Team) (routing.Refresh, error) {
		_, uerr := routing.Update(stateDir, "default", routing.Expect{Account: alice}, func(s *routing.State) error {
			e2, _ := s.Team("e2")
			p := e2.Policy
			p.Revision = 5
			p.FetchedAt = now.Add(3 * time.Minute)
			_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: "e2", Epoch: e2.Epoch, Policy: &p, At: now})
			return err
		})
		if uerr != nil {
			t.Fatalf("concurrent e2 refresh: %v", uerr)
		}
		return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Failure: routing.OrgAccessLost, At: now}, nil
	}
	if err := ref.Refresh(t.Context(), "e1"); err != nil {
		t.Fatalf("org access lost refresh err: %v", err)
	}
	st, _, _ = routing.Load(stateDir, "default")
	got1, _ = st.Team("e1")
	got2, _ := st.Team("e2")
	got3, _ := st.Team("e3")
	if got1.StateAt(now) != routing.Refused || got1.LastError != routing.OrgAccessLost {
		t.Fatalf("e1 = %+v; want Refused(OrgAccessLost)", got1)
	}
	if got2.StateAt(now) != routing.Active || got2.Policy.Revision != 5 {
		t.Fatalf("newer-epoch sibling e2 was wrongly refused: %+v", got2)
	}
	if got3.StateAt(now) != routing.Active {
		t.Fatalf("other-org e3 was wrongly refused: %+v", got3)
	}
	if !slices.Equal(changed, []string{"e1"}) {
		t.Fatalf("OrgAccessLost onChange = %v, want [e1]", changed)
	}
}

// Coalesced Nudge refreshes immediately through the same fence even when a refresh is
// already in flight, and BindTeams invalidates cached routing state on commit.
func TestBindTeamsNudgeCoalescesAndInvalidates(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfgDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice := testAccount("alice")
	t1 := activeTeam("e1", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Generation: 1, Teams: []routing.Team{t1}})

	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, Account: alice}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondDone := make(chan struct{})
	var calls atomic.Int32

	d := Deps{
		LoadConfig: func() (*config.Config, error) { return cfg, nil },
		RefreshTeam: func(_ context.Context, _ *config.Config, _ string, tm routing.Team) (routing.Refresh, error) {
			n := calls.Add(1)
			if n == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			p := tm.Policy
			p.Revision = int64(n) + 1
			p.FetchedAt = now.Add(time.Duration(n) * time.Minute)
			if n == 2 {
				defer close(secondDone)
			}
			return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &p, At: now}, nil
		},
	}
	var opts relay.Options
	worker, _ := BindTeams(d, cfg, &opts)
	if opts.Router == nil || opts.NudgeRefresh == nil {
		t.Fatal("BindTeams did not wire Router and NudgeRefresh")
	}
	// Populate Teams cache before refresh so we verify Invalidate clears it.
	if got := opts.Router.Recheck("e1", alice).Generation(); got != 1 {
		t.Fatalf("initial generation = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker(ctx)
	}()

	// Trigger immediate refresh via Nudge even though FetchedAt == now (< 1m).
	opts.NudgeRefresh("e1")
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first nudged refresh did not start")
	}
	// Multiple nudges while in flight must coalesce into exactly one follow-up refresh.
	opts.NudgeRefresh("e1")
	opts.NudgeRefresh("e1")
	opts.NudgeRefresh("e1")
	close(releaseFirst)

	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("coalesced follow-up refresh did not run")
	}
	// Give commit + onChange a moment, then stop worker.
	deadline := time.Now().Add(2 * time.Second)
	for opts.Router.Recheck("e1", alice).Generation() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := calls.Load(); got != 2 {
		t.Fatalf("refresh calls = %d, want 2 (1 initial + 1 coalesced)", got)
	}
	if got := opts.Router.Recheck("e1", alice).Generation(); got != 3 {
		t.Fatalf("generation after invalidated refresh = %d, want 3", got)
	}
}

// PruneRuns keeps expired run ledgers referenced by the .teams/ outbox or events-v2.jsonl,
// prunes unqueued expired ledgers, and skips pruning on any queue scan error.
func TestPruneRunsPreservesOutboxAndSpoolSessions(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	now := time.Now().UTC()

	for _, sid := range []string{"s-outbox", "s-spool", "s-expired"} {
		if _, err := runs.Start(stateDir, sid, old); err != nil {
			t.Fatalf("runs.Start(%s): %v", sid, err)
		}
		p := filepath.Join(stateDir, runs.Dir, sid+".json")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// Put s-spool into events-v2.jsonl.
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(testAccount("alice"))
	if err := sp.Append(spool.Event{Time: now, Name: "session_start", SessionID: "s-spool"}); err != nil {
		t.Fatal(err)
	}

	// Error from queuedOutbox must skip pruning completely.
	PruneRuns(t.Context(), stateDir, func() (map[string]bool, error) {
		return nil, errors.New("permission denied")
	}, now)
	if _, err := os.Stat(filepath.Join(stateDir, runs.Dir, "s-expired.json")); err != nil {
		t.Fatalf("s-expired pruned despite outbox scan error: %v", err)
	}

	// Cancelled ctx makes spool.Sessions fail -> must skip pruning completely.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	PruneRuns(cancelled, stateDir, func() (map[string]bool, error) {
		return map[string]bool{"s-outbox": true}, nil
	}, now)
	if _, err := os.Stat(filepath.Join(stateDir, runs.Dir, "s-expired.json")); err != nil {
		t.Fatalf("s-expired pruned despite spool scan error: %v", err)
	}

	// Clean scan keeps s-outbox and s-spool, prunes s-expired.
	PruneRuns(t.Context(), stateDir, func() (map[string]bool, error) {
		return map[string]bool{"s-outbox": true}, nil
	}, now)
	for _, keep := range []string{"s-outbox", "s-spool"} {
		if _, err := os.Stat(filepath.Join(stateDir, runs.Dir, keep+".json")); err != nil {
			t.Fatalf("queued session %s was pruned: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, runs.Dir, "s-expired.json")); !os.IsNotExist(err) {
		t.Fatalf("unqueued expired session was not pruned: err=%v", err)
	}
}

// Teams fails closed when routing/<profile>.json is corrupt or belongs to another account,
// handles interval Mixed on Route and Recheck, and resolves user-token vs server-key
// policies strictly against account, team, and environment.
func TestTeamsActiveMixedAndResolve(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfgDir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	cfg := &config.Config{
		Dir: cfgDir, StateDir: stateDir, ProfileName: "default",
		AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: alice,
	}
	teams := NewTeams(cfg, TeamsDeps{
		Now:       func() time.Time { return now },
		AgentName: func(tool string) string { return tool },
		User: func(_ *config.Config, _ routing.Team, _ string) string {
			return "alice"
		},
	})

	// Missing routing file -> Active() is false (pre-cutover fallback).
	if teams.Active() {
		t.Fatal("Active() = true without routing file")
	}

	// Corrupt routing file -> Active() is true (fails closed, no fallback), Route holds with WhyNoState.
	path, err := routing.StatePath(stateDir, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	teams.Invalidate()
	if !teams.Active() {
		t.Fatal("Active() = false on corrupt routing file; must fail closed")
	}
	if got := teams.Route(relay.Record{Account: alice}); got.Outcome != relay.Hold || got.Why != relay.WhyNoState {
		t.Fatalf("Route on corrupt file = %+v, want Hold(WhyNoState)", got)
	}

	// Valid user-token routing state + interval Mixed check.
	tm := activeTeam("e1", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Teams: []routing.Team{tm}})
	teams.Invalidate()

	if _, err := runs.Start(stateDir, "sess-mixed", now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mixedAt := now.Add(-2 * time.Minute)
	if err := runs.MarkMixed(stateDir, "sess-mixed", mixedAt, runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	// Straddling interval [mixedAt-1m, mixedAt+1m] MUST drop as Mixed!
	straddle := relay.Record{
		Session: "sess-mixed", Account: alice, Placed: true,
		Placement: claim.Claim{Profile: "default", Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/a/r1"}},
		At:        mixedAt.Add(-time.Minute), EndAt: mixedAt.Add(time.Minute),
	}
	if got := teams.Route(straddle); got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(mixedAt) {
		t.Fatalf("straddling interval Route = %+v, want Drop(WhyMixed, %v)", got, mixedAt)
	}
	// Before binding: pre-mixed record holds with destination pinned to e1.
	before := straddle
	before.At, before.EndAt = mixedAt.Add(-3*time.Minute), mixedAt.Add(-time.Minute)
	pinned := teams.Route(before)
	if pinned.Outcome != relay.Hold || pinned.Why != relay.WhyPending || pinned.Queued.Enrollment != "e1" {
		t.Fatalf("unbound pre-mixed Route = %+v, want Hold(WhyPending) pinned to e1", pinned)
	}
	// After binding repo to org-a: pre-mixed record routes to Send!
	saved := writeRoutingState(t, stateDir, routing.State{
		Profile:  "default",
		Account:  alice,
		Teams:    []routing.Team{tm},
		Bindings: map[string]routing.Binding{"github.com/a/r1": {Organization: testOrgA}},
	})
	teams.Invalidate()
	routed := teams.Route(before)
	if routed.Outcome != relay.Send || routed.Queued.Enrollment != "e1" || routed.Queued.Account != alice {
		t.Fatalf("pre-mixed Route = %+v, want Send(e1, alice)", routed)
	}
	// Recheck on straddling queued record -> Discard(WhyMixed, MixedAt).
	rc := teams.Recheck("e1", alice)
	if rc.Generation() != saved.Generation {
		t.Fatalf("Recheck.Generation = %d, want %d", rc.Generation(), saved.Generation)
	}
	qStraddle := routed.Queued
	qStraddle.EndAt = mixedAt.Add(time.Second)
	if ver := rc.Entry(qStraddle); ver.Verdict != relay.Discard || ver.Why != relay.WhyMixed || !ver.MixedAt.Equal(mixedAt) {
		t.Fatalf("Recheck straddling = %+v, want Discard(WhyMixed)", ver)
	}

	// Resolve for user-token profile: pol.Key == "", pol.User == "alice", pol.UserAccount == alice.
	pol, err := teams.Resolve("e1", "claude", alice)
	if err != nil {
		t.Fatalf("user Resolve: %v", err)
	}
	if pol.Key != "" || pol.User != "alice" || pol.UserAccount != alice || pol.Account != alice {
		t.Fatalf("user Resolve policy = %+v", pol)
	}
	// Account mismatch fails closed.
	if _, err := teams.Resolve("e1", "claude", testAccount("bob")); err == nil {
		t.Fatal("Resolve succeeded for wrong account")
	}

	// Server-key profile Resolve: validates cfg.Team & cfg.ServerKeyAuthURL, loads keys.json.
	srvDir := t.TempDir()
	srvState := t.TempDir()
	setupProfileDir(t, srvDir, "default", []string{"claude"})
	if err := keystore.Set(srvDir, "team-srv", mintedKey, keystore.Hosts{OTLP: testOTLPURL, API: testAuthURL}); err != nil {
		t.Fatal(err)
	}
	srvTeam := activeTeam("e-srv", "team-srv", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, srvState, routing.State{Profile: "default", Account: sendas.NoPersonLane, Generation: 1, Teams: []routing.Team{srvTeam}})

	srvCfg := &config.Config{
		Dir: srvDir, StateDir: srvState, ProfileName: "default",
		AuthURL: testAuthURL, OTLPURL: testOTLPURL,
		ServerKeySignIn: true, Team: "team-srv", ServerKeyAuthURL: testAuthURL,
	}
	srvTeams := NewTeams(srvCfg, TeamsDeps{Now: func() time.Time { return now }, AgentName: func(s string) string { return s }})
	srvPol, err := srvTeams.Resolve("e-srv", "claude", sendas.NoPersonLane)
	if err != nil {
		t.Fatalf("server-key Resolve: %v", err)
	}
	if srvPol.Key != mintedKey || srvPol.User != "" || srvPol.Account != sendas.NoPersonLane {
		t.Fatalf("server-key Resolve policy = %+v", srvPol)
	}
	// Mismatched ServerKeyAuthURL fails closed.
	badAuthCfg := *srvCfg
	badAuthCfg.ServerKeyAuthURL = "https://other.example"
	badAuthTeams := NewTeams(&badAuthCfg, TeamsDeps{Now: func() time.Time { return now }})
	if _, err := badAuthTeams.Resolve("e-srv", "claude", sendas.NoPersonLane); err == nil {
		t.Fatal("server-key Resolve succeeded with mismatched ServerKeyAuthURL")
	}
	// Mismatched cfg.Team fails closed.
	badTeamCfg := *srvCfg
	badTeamCfg.Team = "team-other"
	badTeamTeams := NewTeams(&badTeamCfg, TeamsDeps{Now: func() time.Time { return now }})
	if _, err := badTeamTeams.Resolve("e-srv", "claude", sendas.NoPersonLane); err == nil {
		t.Fatal("server-key Resolve succeeded with mismatched cfg.Team")
	}
}
