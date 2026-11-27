package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/policy"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
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

	// Missing routing file -> Active() is true (cutover fails closed, zero fallback).
	if !teams.Active() {
		t.Fatal("Active() = false without routing file; must fail closed")
	}
	if got := teams.Route(relay.Record{Account: alice}); got.Outcome != relay.Hold || got.Why != relay.WhyNoState {
		t.Fatalf("Route on missing file = %+v, want Hold(WhyNoState)", got)
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
	saved := writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Teams: []routing.Team{tm}})
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
		Placement: claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/a/r1"}},
		At:        mixedAt.Add(-time.Minute), EndAt: mixedAt.Add(time.Minute),
	}
	if got := teams.Route(straddle); got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(mixedAt) {
		t.Fatalf("straddling interval Route = %+v, want Drop(WhyMixed, %v)", got, mixedAt)
	}
	// A pre-mixed record routes to Send.
	before := straddle
	before.At, before.EndAt = mixedAt.Add(-3*time.Minute), mixedAt.Add(-time.Minute)
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

type fakeSignIn struct {
	account string
	tokenFn func(ctx context.Context, account string) (string, error)
	refused atomic.Int32
	retired atomic.Int32
}

func (f *fakeSignIn) Account() string        { return f.account }
func (f *fakeSignIn) Lane() (string, uint64) { return f.account, 1 }
func (f *fakeSignIn) Token(ctx context.Context, account string) (string, error) {
	if f.tokenFn != nil {
		return f.tokenFn(ctx, account)
	}
	if account != f.account {
		return "", nil
	}
	return "tok-" + account, nil
}
func (f *fakeSignIn) Refused(context.Context, string) { f.refused.Add(1) }
func (f *fakeSignIn) Retired(string)                  { f.retired.Add(1) }

type sessionCorrelator struct{}

func (sessionCorrelator) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

func sessionLogBytes(t *testing.T, session, body string, at time.Time) []byte {
	t.Helper()
	b, err := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key:   "session.id",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: session}},
		}}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(at.UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		}}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Surviving foreign-account (or empty-account) placements fail closed after Alice->Bob replacement.
func TestTeamsRouteRejectsForeignPlacementAccount(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice, bob := testAccount("alice"), testAccount("bob")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	tm := activeTeam("e-bob", "team-bob", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{
		Profile: "default",
		Account: bob,
		Teams:   []routing.Team{tm},
	})
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: bob}
	teams := NewTeams(cfg, TeamsDeps{Now: func() time.Time { return now }})

	for name, claimAcct := range map[string]string{"foreign_alice": alice, "empty_account": ""} {
		got := teams.Route(relay.Record{
			Session: "s1", Account: bob, Placed: true, At: now, EndAt: now,
			Placement: claim.Claim{Profile: "default", Account: claimAcct, Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/a/r1"}},
		})
		if got.Outcome != relay.Hold || got.Why != relay.WhyNoState {
			t.Fatalf("%s: Route = %+v, want Hold(WhyNoState)", name, got)
		}
	}
	// Matching Bob placement routes cleanly.
	ok := teams.Route(relay.Record{
		Session: "s1", Account: bob, Placed: true, At: now, EndAt: now,
		Placement: claim.Claim{Profile: "default", Account: bob, Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/a/r1"}},
	})
	if ok.Outcome != relay.Send || ok.Queued.Enrollment != "e-bob" {
		t.Fatalf("bob placement Route = %+v, want Send(e-bob)", ok)
	}
}

// Intervals spanning a Mixed run followed by a clean resumed run are caught by Route and Recheck.
func TestTeamsMixedIntervalSpanningCleanResume(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	now := t0.Add(time.Minute)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	tm := activeTeam("e1", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{
		Profile: "default",
		Account: alice,
		Teams:   []routing.Team{tm},
	})
	// Run 1 starts at t0, turns Mixed at t0+10s; Run 2 resumes cleanly at t0+20s.
	if _, err := runs.Start(stateDir, "sess-resume", t0); err != nil {
		t.Fatal(err)
	}
	mixedAt := t0.Add(10 * time.Second)
	if err := runs.MarkMixed(stateDir, "sess-resume", mixedAt, runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Start(stateDir, "sess-resume", t0.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: alice}
	teams := NewTeams(cfg, TeamsDeps{Now: func() time.Time { return now }})
	base := relay.Record{
		Session: "sess-resume", Account: alice, Placed: true,
		Placement: claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/a/r1"}},
	}

	// Span/metric [t0+5s, t0+25s] starts before MixedAt and ends inside clean Run 2 -> MUST Drop!
	spanning := base
	spanning.At, spanning.EndAt = t0.Add(5*time.Second), t0.Add(25*time.Second)
	if got := teams.Route(spanning); got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(mixedAt) {
		t.Fatalf("spanning resume Route = %+v, want Drop(WhyMixed, %v)", got, mixedAt)
	}
	// Clean pre-Mixed [t0+5s, t0+9s] and clean post-resume [t0+21s, t0+25s] MUST Send!
	for name, iv := range map[string][2]time.Time{
		"before_mixed": {t0.Add(5 * time.Second), t0.Add(9 * time.Second)},
		"after_resume": {t0.Add(21 * time.Second), t0.Add(25 * time.Second)},
	} {
		rec := base
		rec.At, rec.EndAt = iv[0], iv[1]
		if got := teams.Route(rec); got.Outcome != relay.Send {
			t.Fatalf("%s Route = %+v, want Send", name, got)
		}
	}
	// Recheck catches spanning queued entry too.
	rc := teams.Recheck("e1", alice)
	q := relay.Queued{
		Profile: "default", Enrollment: "e1", Project: "team-1", Account: alice,
		Kind: string(routing.Repository), Repository: "github.com/a/r1",
		Session: "sess-resume", At: t0.Add(5 * time.Second), EndAt: t0.Add(25 * time.Second),
	}
	if ver := rc.Entry(q); ver.Verdict != relay.Discard || ver.Why != relay.WhyMixed || !ver.MixedAt.Equal(mixedAt) {
		t.Fatalf("spanning resume Recheck = %+v, want Discard(WhyMixed, %v)", ver, mixedAt)
	}
}

// Route names the run its Mixed cutoff belongs to, which the relay's sweep is scoped by: the
// paused run for a resumed process's record stamped before its new run began (placed in the
// new run), and a Mixed placement's own run when the ledger has no crossing for the record.
func TestTeamsRouteNamesTheMixedRun(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	now := t0.Add(time.Minute)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Teams: []routing.Team{activeTeam("e1", "team-1", testOrgA, false, now, "github.com/a/r1")}})
	paused, err := runs.Start(stateDir, "sess-pause", t0)
	if err != nil {
		t.Fatal(err)
	}
	mixedAt := t0.Add(10 * time.Second)
	if err := runs.MarkMixed(stateDir, "sess-pause", mixedAt, runs.Why{Reason: runs.UnknownReach}); err != nil {
		t.Fatal(err)
	}
	resumed, err := runs.Start(stateDir, "sess-pause", t0.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: alice}
	teams := NewTeams(cfg, TeamsDeps{Now: func() time.Time { return now }})
	rec := relay.Record{
		Session: "sess-pause", Account: alice, Placed: true, At: t0.Add(19 * time.Second),
		Placement: claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Run: resumed, Repository: config.Repository{Origin: "github.com/a/r1"}},
	}
	if got := teams.Route(rec); got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(mixedAt) || got.MixedRun != paused {
		t.Fatalf("stale record Route = %+v, want Drop(WhyMixed, %v) of run %s", got, mixedAt, paused)
	}
	rec.At = t0.Add(21 * time.Second)
	if got := teams.Route(rec); got.Outcome != relay.Send || got.Queued.Run != resumed {
		t.Fatalf("resumed record Route = %+v, want Send under run %s", got, resumed)
	}
	rec.Placement.Kind = claim.KindMixed
	if got := teams.Route(rec); got.Outcome != relay.Drop || got.Why != relay.WhyMixed || !got.MixedAt.Equal(rec.At) || got.MixedRun != resumed {
		t.Fatalf("Mixed placement Route = %+v, want Drop(WhyMixed, %v) of run %s", got, rec.At, resumed)
	}
}

// A ledger read cached before a resumed process's session-start opened its new run does not
// judge that run's records: one placed in a run the read does not hold reads the ledger again,
// so the resumed prompt is sent rather than dropped against the paused run, and the read is
// cached for the records after it.
func TestTeamsRouteRereadsALedgerThatPredatesTheRun(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	clock := t0.Add(19 * time.Second)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Teams: []routing.Team{activeTeam("e1", "team-1", testOrgA, false, t0.Add(time.Minute), "github.com/a/r1")}})
	if _, err := runs.Start(stateDir, "sess-pause", t0); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(stateDir, "sess-pause", t0.Add(10*time.Second), runs.Why{Reason: runs.UnknownReach}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: alice}
	teams := NewTeams(cfg, TeamsDeps{Now: func() time.Time { return clock }})
	reads := 0
	teams.readLedger = func(stateDir, id string) runs.Snapshot { reads++; return runs.Read(stateDir, id) }

	// The resumed process's first record arrives before its session-start, unplaced.
	if got := teams.Route(relay.Record{Session: "sess-pause", Account: alice, At: t0.Add(19 * time.Second)}); got.Outcome != relay.Drop || got.Why != relay.WhyMixed {
		t.Fatalf("record before the resume Route = %+v, want Drop(WhyMixed)", got)
	}
	resumed, err := runs.Start(stateDir, "sess-pause", t0.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	rec := relay.Record{
		Session: "sess-pause", Account: alice, Placed: true, At: t0.Add(20*time.Second + 300*time.Millisecond),
		Placement: claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Run: resumed, Repository: config.Repository{Origin: "github.com/a/r1"}},
	}
	clock = t0.Add(19*time.Second + 500*time.Millisecond) // within ledgerTTL of the first read
	if got := teams.Route(rec); got.Outcome != relay.Send || got.Queued.Run != resumed {
		t.Fatalf("resumed record Route = %+v, want Send under run %s", got, resumed)
	}
	clock = clock.Add(100 * time.Millisecond)
	if got := teams.Route(rec); got.Outcome != relay.Send {
		t.Fatalf("next resumed record Route = %+v, want Send", got)
	}
	if reads != 2 {
		t.Fatalf("ledger read %d times, want 2: once at first, once more for the run the first read predates", reads)
	}
}

// Pre-POST send fence reads fresh routing state even without Invalidate when CAS deselects
// an enrollment while Token acquisition is blocked (within the 1s route cache TTL).
func TestTeamsSendFenceReadsFreshStateWithoutInvalidate(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir, outboxDir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	var posts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	tm := activeTeam("e1", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{
		Profile: "default",
		Account: alice,
		Teams:   []routing.Team{tm},
	})
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: up.URL, Account: alice}
	// Fixed clock keeps Route's 1s cache perpetually fresh if Recheck doesn't bypass it.
	teams := NewTeams(cfg, TeamsDeps{Now: func() time.Time { return now }, AgentName: func(s string) string { return s }})

	tokEntered := make(chan struct{})
	tokRelease := make(chan struct{})
	var tokOnce sync.Once
	si := &fakeSignIn{
		account: alice,
		tokenFn: func(_ context.Context, acct string) (string, error) {
			tokOnce.Do(func() { close(tokEntered) })
			<-tokRelease
			return "tok-" + acct, nil
		},
	}

	r := relay.New(relay.Options{
		Dir: outboxDir, Token: "relay-tok", SignIn: si, Router: teams,
		Correlators: []shape.Correlator{sessionCorrelator{}},
		Now:         func() time.Time { return now },
		Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
			if session != "s-fence" {
				return claim.Claim{}, false
			}
			return claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Tool: "claude", Repository: config.Repository{Origin: "github.com/a/r1"}}, true
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	defer func() { cancel(); <-done }()

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(sessionLogBytes(t, "s-fence", "hello", now)))
	req.Header.Set("Authorization", "Bearer relay-tok")
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d", rec.Code)
	}

	select {
	case <-tokEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for teamSender to enter Token()")
	}
	// Deselect e1 via CAS WITHOUT calling teams.Invalidate() and with frozen clock!
	if _, err := routing.Update(stateDir, "default", routing.Expect{Account: alice}, func(s *routing.State) error {
		s.Deselect("e1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(tokRelease)

	deadline := time.Now().Add(3 * time.Second)
	for r.Stats().Snapshot().Counters["dropped.team_deselected.logs"] < 1 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if posts.Load() != 0 {
		t.Fatalf("upstream received %d POSTs after concurrent CAS deselect, want 0", posts.Load())
	}
	if got := r.Stats().Snapshot().Counters["dropped.team_deselected.logs"]; got != 1 {
		t.Fatalf("dropped.team_deselected.logs = %d, want 1", got)
	}
}

// PolicyRefresher recalculates cached deadlines when Every changes (sibling refusal 1m->24h
// and external Refused->Active recovery 24h->1m) while preserving failure throttling.
func TestPolicyRefresherRecalculatesDeadlinesOnStateTransitions(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	getNow := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	everyMap := map[string]time.Duration{"e-sib": PolicyRefreshInterval}
	fetchedMap := map[string]time.Time{"e-sib": now}
	refreshes := make(chan string, 10)
	tickCh := make(chan time.Time, 1)

	ref := &PolicyRefresher{
		Interval: PolicyRefreshInterval,
		Discover: time.Second,
		Now:      getNow,
		After:    func(time.Duration) <-chan time.Time { return tickCh },
		Teams:    func() []string { return []string{"e-sib"} },
		Every: func(team string) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return everyMap[team]
		},
		Fetched: func(team string) time.Time {
			mu.Lock()
			defer mu.Unlock()
			return fetchedMap[team]
		},
		Refresh: func(_ context.Context, team string) error {
			refreshes <- team
			return errors.New("transient")
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); ref.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Initial due() caches 1m deadline (now + 1m). Now sibling is refused org-wide (24h).
	tickCh <- getNow()
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	everyMap["e-sib"] = RefusedRecheck
	fetchedMap["e-sib"] = now
	mu.Unlock()

	// Advancing 2m must NOT trigger a refresh because the sibling adopted RefusedRecheck (24h).
	advance(2 * time.Minute)
	tickCh <- getNow()
	select {
	case got := <-refreshes:
		t.Fatalf("refused sibling %q refreshed after 2m despite 24h RefusedRecheck", got)
	case <-time.After(80 * time.Millisecond):
	}

	// External recovery back to Active (1m) with an older FetchedAt triggers refresh on next tick!
	mu.Lock()
	everyMap["e-sib"] = PolicyRefreshInterval
	fetchedMap["e-sib"] = now.Add(-2 * time.Minute)
	mu.Unlock()
	tickCh <- getNow()
	select {
	case <-refreshes:
	case <-time.After(2 * time.Second):
		t.Fatal("externally recovered Active team did not recalculate past 24h deadline")
	}

	// Transient failure with unchanged 1m interval preserves failure throttling (no immediate spin).
	time.Sleep(30 * time.Millisecond)
	tickCh <- getNow()
	select {
	case got := <-refreshes:
		t.Fatalf("failed refresh for %q spun immediately instead of throttling", got)
	case <-time.After(80 * time.Millisecond):
	}
}

// User identity cache is scoped by account so Alice->Bob switch in the same org stamps Bob.
func TestTeamsUserCacheScopedByAccount(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice, bob := testAccount("alice"), testAccount("bob")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	var mu sync.Mutex
	curAcct, curUser := alice, "alice"
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: testOTLPURL, Account: alice}
	teams := NewTeams(cfg, TeamsDeps{
		Now: func() time.Time { return now },
		Profile: func(string) (*config.Config, error) {
			mu.Lock()
			defer mu.Unlock()
			c := *cfg
			c.Account = curAcct
			return &c, nil
		},
		User: func(_ *config.Config, _ routing.Team, _ string) string {
			mu.Lock()
			defer mu.Unlock()
			return curUser
		},
	})

	tAlice := activeTeam("e-a", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: alice, Teams: []routing.Team{tAlice}})
	polA, err := teams.Resolve("e-a", "claude", alice)
	if err != nil || polA.User != "alice" || polA.UserAccount != alice {
		t.Fatalf("alice Resolve = %+v, %v", polA, err)
	}

	// Switch to Bob in the SAME org, profile, authURL, and endpoint within userTTL!
	mu.Lock()
	curAcct, curUser = bob, "bob"
	mu.Unlock()
	tBob := activeTeam("e-b", "team-1", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{Profile: "default", Account: bob, Teams: []routing.Team{tBob}})
	polB, err := teams.Resolve("e-b", "claude", bob)
	if err != nil || polB.User != "bob" || polB.UserAccount != bob {
		t.Fatalf("bob Resolve reused stale cache: %+v, %v", polB, err)
	}
}

// End-to-end 403 ORG_NOT_ACCESSIBLE -> coalesced Nudge -> CAS ApplyOrgAccessLost ->
// OnStateChange wakes teamSender immediately past refusedBackoff and purges the dead queue.
func TestBindTeams403RefreshesCommitsRefusedAndPurgesQueue(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir, outboxDir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice := testAccount("alice")
	setupProfileDir(t, cfgDir, "default", []string{"claude"})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":7,"message":"denied","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`))
	}))
	defer up.Close()

	tm := activeTeam("e-403", "team-403", testOrgA, false, now, "github.com/a/r1")
	writeRoutingState(t, stateDir, routing.State{
		Profile: "default",
		Account: alice,
		Teams:   []routing.Team{tm},
	})
	cfg := &config.Config{Dir: cfgDir, StateDir: stateDir, ProfileName: "default", AuthURL: testAuthURL, OTLPURL: up.URL, Account: alice}

	var refreshed atomic.Int32
	d := Deps{
		LoadConfig: func() (*config.Config, error) { return cfg, nil },
		AgentName:  func(s string) string { return s },
		RefreshTeam: func(_ context.Context, _ *config.Config, _ string, team routing.Team) (routing.Refresh, error) {
			refreshed.Add(1)
			return routing.Refresh{
				EnrollmentID: team.EnrollmentID, Epoch: team.Epoch,
				Failure: routing.OrgAccessLost, At: time.Now().UTC(),
			}, nil
		},
	}

	si := &fakeSignIn{account: alice}
	opts := relay.Options{
		Dir: outboxDir, Token: "relay-tok", SignIn: si,
		Correlators: []shape.Correlator{sessionCorrelator{}},
		Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
			if session != "s-403" {
				return claim.Claim{}, false
			}
			return claim.Claim{Profile: "default", Account: alice, Kind: claim.KindRepository, Tool: "claude", Repository: config.Repository{Origin: "github.com/a/r1"}}, true
		},
	}
	worker, onRelay := BindTeams(d, cfg, &opts)
	r := relay.New(opts)
	onRelay(r)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { worker(ctx) })
	wg.Go(func() { r.Run(ctx) })
	defer func() { cancel(); wg.Wait() }()

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(sessionLogBytes(t, "s-403", "hello", now)))
	req.Header.Set("Authorization", "Bearer relay-tok")
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d", rec.Code)
	}

	// Must refresh, commit Refused, wake past 5m refusedBackoff, and purge within 5s.
	deadline := time.Now().Add(5 * time.Second)
	for r.Stats().Snapshot().Counters["dropped.team_blocked.logs"] < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := r.Stats().Snapshot().Counters["dropped.team_blocked.logs"]; got != 1 {
		t.Fatalf("dropped.team_blocked.logs = %d (refreshed=%d, counters=%v), want 1", got, refreshed.Load(), r.Stats().Snapshot().Counters)
	}
	if si.refused.Load() != 0 || si.retired.Load() != 0 {
		t.Fatalf("403 ORG_NOT_ACCESSIBLE mutated sign-in: refused=%d retired=%d", si.refused.Load(), si.retired.Load())
	}
	st, ok, err := routing.Load(stateDir, "default")
	if err != nil || !ok {
		t.Fatalf("routing.Load: %v, %v", ok, err)
	}
	gotTeam, _ := st.Team("e-403")
	if gotTeam.StateAt(time.Now().UTC()) != routing.Refused || gotTeam.LastError != routing.OrgAccessLost {
		t.Fatalf("team state = %+v, want Refused(OrgAccessLost)", gotTeam)
	}
	queued, err := r.QueuedSessions()
	if err != nil || len(queued) != 0 {
		t.Fatalf("outbox not purged: queued=%v err=%v", queued, err)
	}
}

// TeamRefresher enumerates and refreshes enrollments from every account-verified profile
// (not just the active profile), so queued outbox entries from a previous profile continue
// refreshing after switching active profiles while mismatched profiles still fail closed.
func TestTeamRefresherRefreshesNonActiveProfileAfterSwitch(t *testing.T) {
	t.Parallel()
	stateDir, cfgDir := t.TempDir(), t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	alice, bob := testAccount("alice"), testAccount("bob")

	for name, acct := range map[string]string{
		"work":       alice,
		"home":       alice,
		"mismatched": bob,
		"fresh":      alice,
	} {
		if err := config.UpdateProfile(cfgDir, name, func(p *config.Profile) {
			p.Account = acct
			p.AuthURL = testAuthURL
			p.OTLPURL = testOTLPURL
		}); err != nil {
			t.Fatalf("UpdateProfile(%s): %v", name, err)
		}
	}

	tWork := activeTeam("e-work", "team-work", testOrgA, false, now.Add(-2*time.Minute), "github.com/a/r1")
	tWorkRef := activeTeam("e-work-ref", "team-work-ref", testOrgA, false, now.Add(-time.Hour), "github.com/a/r2")
	tWorkRef.Refusals = 2
	tWorkRef.RefusedAt = now.Add(-3 * time.Hour)
	writeRoutingState(t, stateDir, routing.State{Profile: "work", Account: alice, Teams: []routing.Team{tWork, tWorkRef}})

	tHome := activeTeam("e-home", "team-home", testOrgB, false, now.Add(-2*time.Minute), "github.com/b/r1")
	writeRoutingState(t, stateDir, routing.State{Profile: "home", Account: alice, Teams: []routing.Team{tHome}})

	// Mismatched profile: routing/mismatched.json belongs to alice, config says bob -> must fail closed.
	tBad := activeTeam("e-mismatch", "team-bad", testOrgB, false, now.Add(-2*time.Minute), "github.com/b/bad")
	writeRoutingState(t, stateDir, routing.State{Profile: "mismatched", Account: alice, Teams: []routing.Team{tBad}})

	activeName := "home"
	var mu sync.Mutex
	var refreshedProfiles []string
	var changed []string
	d := Deps{
		LoadConfig: func() (*config.Config, error) {
			mu.Lock()
			name := activeName
			mu.Unlock()
			return config.Load(cfgDir, stateDir, config.Overrides{Profile: name})
		},
		LoadProfile: func(name string) (*config.Config, error) {
			return config.Load(cfgDir, stateDir, config.Overrides{Profile: name})
		},
		RefreshTeam: func(_ context.Context, cfg *config.Config, acct string, tm routing.Team) (routing.Refresh, error) {
			mu.Lock()
			refreshedProfiles = append(refreshedProfiles, cfg.ProfileName+":"+tm.EnrollmentID+":"+acct)
			mu.Unlock()
			p := tm.Policy
			p.Revision++
			p.FetchedAt = now
			return routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Policy: &p, At: now}, nil
		},
	}
	ref := d.TeamRefresher(func(ids ...string) {
		mu.Lock()
		changed = append(changed, ids...)
		mu.Unlock()
	})

	// Active profile is "home"; "work" enrollments must still be discovered and "mismatched" excluded.
	if got := ref.Teams(); !slices.Equal(got, []string{"e-home", "e-work", "e-work-ref"}) {
		t.Fatalf("Teams() = %v, want [e-home e-work e-work-ref]", got)
	}
	if got := ref.Every("e-work-ref"); got != RefusedRecheck {
		t.Fatalf("Every(e-work-ref) = %v, want %v", got, RefusedRecheck)
	}
	if !ref.Fetched("e-work-ref").Equal(tWorkRef.RefusedAt) {
		t.Fatalf("Fetched(e-work-ref) = %v, want %v", ref.Fetched("e-work-ref"), tWorkRef.RefusedAt)
	}

	// Refreshing non-active profile "work" uses work's config, commits CAS update, stores policy, and notifies onChange.
	if err := ref.Refresh(t.Context(), "e-work"); err != nil {
		t.Fatalf("Refresh(e-work): %v", err)
	}
	if err := ref.Refresh(t.Context(), "e-mismatch"); err != nil {
		t.Fatalf("Refresh(e-mismatch): %v", err)
	}
	mu.Lock()
	gotRefreshed := slices.Clone(refreshedProfiles)
	gotChanged := slices.Clone(changed)
	mu.Unlock()
	if !slices.Equal(gotRefreshed, []string{"work:e-work:" + alice}) {
		t.Fatalf("refreshed = %v, want [work:e-work:%s]", gotRefreshed, alice)
	}
	if !slices.Equal(gotChanged, []string{"e-work"}) {
		t.Fatalf("onChange = %v, want [e-work]", gotChanged)
	}
	workState, ok, err := routing.Load(stateDir, "work")
	if err != nil || !ok {
		t.Fatalf("routing.Load(work): ok=%v err=%v", ok, err)
	}
	gotWork, _ := workState.Team("e-work")
	if gotWork.Policy.Revision != 2 || !gotWork.Policy.FetchedAt.Equal(now) {
		t.Fatalf("updated work team = %+v, want Revision=2 FetchedAt=%v", gotWork.Policy, now)
	}
	if pol, ok, err := config.ReadPolicy(stateDir, "team-work"); err != nil || !ok || pol.Revision != 2 {
		t.Fatalf("stored policy for team-work: ok=%v err=%v rev=%d", ok, err, pol.Revision)
	}

	// Switching active profile to "fresh" (which has no routing state) still discovers "home" and "work".
	mu.Lock()
	activeName = "fresh"
	mu.Unlock()
	if got := ref.Teams(); !slices.Equal(got, []string{"e-home", "e-work", "e-work-ref"}) {
		t.Fatalf("Teams() with fresh active profile = %v, want [e-home e-work e-work-ref]", got)
	}
}
