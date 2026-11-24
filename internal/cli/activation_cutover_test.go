package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// Missing, corrupt, or wrong-account routing.State fails closed across hooks, relay,
// and spool delivery with zero fallback to Profile.Team.
func TestRoutingStateFailClosedAcrossHooksRelayAndDelivery(t *testing.T) {
	cases := []struct {
		name       string
		breakState func(t *testing.T, stateDir, authURL, activeAcct string)
	}{
		{
			name: "missing",
			breakState: func(t *testing.T, stateDir, _, _ string) {
				p, err := routing.StatePath(stateDir, config.DefaultProfile)
				if err != nil {
					t.Fatal(err)
				}
				_ = os.Remove(p)
			},
		},
		{
			name: "corrupt",
			breakState: func(t *testing.T, stateDir, _, _ string) {
				p, err := routing.StatePath(stateDir, config.DefaultProfile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong_account",
			breakState: func(t *testing.T, stateDir, authURL, _ string) {
				otherAcct := auth.AccountKey(authURL, "u-other")
				pol := config.Policy{Mode: config.ModeRepo, Repositories: []string{appRepo.Origin},
					TeamID: acmeWeb, OrganizationID: orgA().ID, AuthURL: authURL, Revision: 1, FetchedAt: time.Now()}
				if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, otherAcct, func(s *routing.State) error {
					if _, err := s.Select(routing.Team{OrganizationID: orgA().ID, AuthURL: authURL, TeamID: acmeWeb, Policy: pol}); err != nil {
						return err
					}
					return s.Bind(appRepo.Origin, orgA().ID, false)
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ingest := pairingSandbox(t)
			t.Setenv("TERMA_POLICY_STUB", "")
			activeAcct := auth.AccountKey(f.srv.URL, "u-1")
			st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
			if err != nil || !found || len(st.Teams) != 1 {
				t.Fatalf("seed routing state: found=%v err=%v", found, err)
			}
			enrID := st.Teams[0].EnrollmentID
			// Break routing.State while Profile.Team remains set to acmeWeb.
			tc.breakState(t, testApp.stateDir, f.srv.URL, activeAcct)

			// 1. Hooks fail closed: Routing is unusable, Team is empty, zero fallback to Profile.Team.
			hp := testApp.hookProfile()
			if hp.Routing.Usable() || hp.Team != "" || hp.Account != sendas.AccountUnreadable {
				t.Fatalf("hookProfile = usable:%v team:%q account:%q, want fail-closed", hp.Routing.Usable(), hp.Team, hp.Account)
			}

			// 2. Relay fails closed: Router.Route holds with WhyNoState (never Send) and heartbeat refuses.
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			dec := testApp.relayDeps().Teams(cfg).Route(relay.Record{
				Placed: true,
				Placement: claim.Claim{
					Profile:    config.DefaultProfile,
					Account:    activeAcct,
					ProjectID:  acmeWeb,
					Tool:       "codex",
					Repository: appRepo,
				},
				Account: activeAcct,
				At:      time.Now(),
			})
			if dec.Outcome != relay.Hold || dec.Why != relay.WhyNoState {
				t.Fatalf("relay Route = %+v, want Hold/WhyNoState", dec)
			}
			if err := testHeartbeatOpts(t).HeartbeatSend(t.Context(), emptyBeat()); err == nil {
				t.Fatalf("relay HeartbeatSend = nil, want fail-closed error")
			} else if tc.name != "corrupt" && !errors.Is(err, relay.ErrNoKey) {
				t.Fatalf("relay HeartbeatSend = %v, want ErrNoKey", err)
			}
			if got := ingest.take(); len(got) != 0 {
				t.Fatalf("relay HeartbeatSend sent %d requests despite %s routing.State", len(got), tc.name)
			}

			// 3. Delivery fails closed: both an enrollment-tagged event and a legacy AttrProjectID event send nothing.
			s := spoolForTest(t)
			if err := s.Append(spool.Event{
				Name: "terma.commit", Time: time.Now(), Repository: appRepo, Account: activeAcct,
				Profile: config.DefaultProfile, Enrollment: enrID,
				Attrs: map[string]any{hookrun.AttrProjectID: acmeWeb},
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Append(spool.Event{
				Name: "terma.commit", Time: time.Now(), Repository: appRepo, Account: activeAcct,
				Attrs: map[string]any{hookrun.AttrProjectID: acmeWeb},
			}); err != nil {
				t.Fatal(err)
			}
			res, err := testApp.flushSpool(t.Context(), true, 0)
			if err != nil {
				t.Fatal(err)
			}
			if res.Sent != 0 || len(ingest.take()) != 0 {
				t.Fatalf("delivery sent %d events despite %s routing.State", res.Sent, tc.name)
			}
		})
	}
}

// --desktop-grant -y --org --team ticks ONLY --team (preserving already-ticked teams for
// the same account), accepts binding defaults under -y, and NEVER enables Personal.
func TestDesktopGrantTicksOnlyRequestedTeamPreservesExistingAndNeverEnablesPersonal(t *testing.T) {
	gateway := grantSandbox(t)
	personalOrgEntry := organization{ID: "33333333-3333-4333-8333-333333333333", Name: "Personal", Role: "owner", Type: "personal"}
	gateway.orgs = []organization{orgA(), orgB(), personalOrgEntry}
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	gateway.grant("grant-1", deskUser)

	// First grant: only teamA is ticked; binding accepted under -y; Personal NOT enabled.
	stdin1 := identityLine(deskUser, orgA().ID) + `{"code":"grant-1"}` + "\n"
	if out, stderr, err := desktopSetup(t, stdin1, orgA().ID, teamA); err != nil {
		t.Fatalf("desktop setup 1: %v\n%s%s", err, out, stderr)
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load 1: found=%v err=%v", found, err)
	}
	wantAcct := auth.AccountKey(gateway.srv.URL, deskUser)
	if st.Account != wantAcct {
		t.Fatalf("Account = %q, want %q", st.Account, wantAcct)
	}
	if len(st.Teams) != 1 || st.Teams[0].TeamID != teamA || st.Teams[0].Personal {
		t.Fatalf("Teams after grant 1 = %+v, want only [%s] (non-personal)", st.Teams, teamA)
	}
	if b, ok := st.Bindings["github.com/acme/app"]; !ok || b.Organization != orgA().ID || b.Refused {
		t.Fatalf("Bindings after grant 1 = %+v, want github.com/acme/app -> %s", st.Bindings, orgA().ID)
	}

	// Second grant (same user, different org/team): preserves teamA, adds ONLY teamB, never enables Personal.
	stdin2 := identityLine(deskUser, orgB().ID)
	if out, stderr, err := desktopSetup(t, stdin2, orgB().ID, teamB); err != nil {
		t.Fatalf("desktop setup 2: %v\n%s%s", err, out, stderr)
	}
	st2, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load 2: found=%v err=%v", found, err)
	}
	var ids []string
	for _, tm := range st2.Teams {
		if tm.Personal {
			t.Fatalf("Personal team was unexpectedly enabled: %+v", tm)
		}
		ids = append(ids, tm.TeamID)
	}
	if !slices.Equal(ids, []string{teamA, teamB}) {
		t.Fatalf("Teams after grant 2 = %v, want [%s %s]", ids, teamA, teamB)
	}
	if st2.Bindings["github.com/acme/app"].Organization != orgA().ID || st2.Bindings["github.com/beta/web"].Organization != orgB().ID {
		t.Fatalf("Bindings after grant 2 = %+v", st2.Bindings)
	}
}

// Server-key setup validates the key's team and environment, keeps the key in keys.json,
// writes a 1-team routing.State with Account: "no_person", and delivers with NO X-Mirador-Project header.
func TestServerKeySetupWritesNoPersonRoutingStateAndOmitsProjectHeader(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	team := projectsIn(orgA().ID)[0].ID
	key := "ter" + "_srv_0123456789abcdef01234567"
	f.serverKeys = map[string]string{key: team}
	f.policyBody = `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`
	ingest := newRecordingIngest(t)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)
	t.Setenv("TERMA_API_KEY", key)

	if out, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err != nil {
		t.Fatalf("server-key setup: %v\n%s", err, out)
	}
	ingest.take()

	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load: found=%v err=%v", found, err)
	}
	if st.Account != sendas.NoPersonLane {
		t.Fatalf("Account = %q, want %q", st.Account, sendas.NoPersonLane)
	}
	if len(st.Teams) != 1 || st.Teams[0].TeamID != team || st.Teams[0].AuthURL != f.srv.URL || st.Teams[0].Personal {
		t.Fatalf("Teams = %+v, want single non-personal team %s at %s", st.Teams, team, f.srv.URL)
	}
	if gotKey, err := keystore.Get(testApp.dir, team); err != nil || gotKey != key {
		t.Fatalf("keystore.Get(%s) = %q, %v", team, gotKey, err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ServerKeySignIn || cfg.ServerKeyAuthURL != f.srv.URL {
		t.Fatalf("ServerKeySignIn=%v ServerKeyAuthURL=%q, want true / %q", cfg.ServerKeySignIn, cfg.ServerKeyAuthURL, f.srv.URL)
	}

	// Unset TERMA_API_KEY and flush a routed event: delivered with stored server key and NO X-Mirador-Project.
	t.Setenv("TERMA_API_KEY", "")
	s := spoolForTest(t)
	appendEvent(t, s, team, time.Now())
	res, err := testApp.flushSpool(t.Context(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	sent := ingest.take()
	if res.Sent != 1 || len(sent) != 1 {
		t.Fatalf("Sent=%d requests=%d, want 1", res.Sent, len(sent))
	}
	if sent[0].auth != "Bearer "+key || sent[0].project != "" {
		t.Fatalf("server-key request auth=%q X-Mirador-Project=%q, want Bearer <key> and empty project header", sent[0].auth, sent[0].project)
	}

	// Environment mismatch (TERMA_AUTH_URL points elsewhere) must fail closed.
	t.Setenv("TERMA_AUTH_URL", "https://other.example.test")
	appendEvent(t, s, team, time.Now())
	res2, err := testApp.flushSpool(t.Context(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Sent != 0 || len(ingest.take()) != 0 {
		t.Fatalf("wrong AuthURL delivered %d events, want 0", res2.Sent)
	}
}

// Activation in terma setup and terma update --refresh refuses to write routing.State
// while a pre-routing relay (!RunInfo.Routing) holds the relay lock.
func TestActivationGatesOnRoutingCapableRelay(t *testing.T) {
	origWait := daemon.GateStopWait
	daemon.GateStopWait = 120 * time.Millisecond
	t.Cleanup(func() { daemon.GateStopWait = origWait })

	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	team := projectsIn(orgA().ID)[0].ID
	key := "ter" + "_srv_fedcba9876543210fedcba98"
	f.serverKeys = map[string]string{key: team}
	t.Setenv("TERMA_API_KEY", key)

	dir := claim.Dir(testApp.stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	infoBytes, _ := json.Marshal(daemon.RunInfo{PID: os.Getpid(), Version: "0.9.0", Routing: false})
	if err := os.WriteFile(filepath.Join(dir, daemon.RunFile), infoBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Setup refuses activation and writes NO routing.State while the pre-routing relay holds the lock.
	if _, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err == nil || !strings.Contains(err.Error(), "pre-routing relay") {
		t.Fatalf("setup with pre-routing relay = %v, want pre-routing relay error", err)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
		t.Fatalf("routing.State was written despite pre-routing relay: found=%v err=%v", found, err)
	}
	// terma update --refresh also refuses while the pre-routing relay holds the lock.
	if _, err := runTerma(t, "update", "--refresh"); err == nil || !strings.Contains(err.Error(), "pre-routing relay") {
		t.Fatalf("update --refresh with pre-routing relay = %v, want pre-routing relay error", err)
	}

	// Once the pre-routing relay releases the lock, setup succeeds and writes routing.State.
	unlock()
	if out, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err != nil {
		t.Fatalf("setup after pre-routing relay exited: %v\n%s", err, out)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || !found {
		t.Fatalf("routing.State missing after gate cleared: found=%v err=%v", found, err)
	}
}

// Cross-user sign-in on the same profile (alice -> bob) replaces routing.State via CAS
// Replace and rejects stale Alice writers with routing.ErrAccountMismatch.
func TestAccountSwitchCASReplaceRejectsStaleAliceWriter(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	const (
		alice = "11111111-1111-4111-8111-aaaaaaaaaaaa"
		bob   = "22222222-2222-4222-8222-bbbbbbbbbbbb"
	)
	gateway.grant("alice-code", alice)
	gateway.grant("bob-code", bob)

	// Alice sets up first.
	if out, stderr, err := desktopSetup(t, identityLine(alice, orgA().ID)+`{"code":"alice-code"}`+"\n", orgA().ID, teamA); err != nil {
		t.Fatalf("alice setup: %v\n%s%s", err, out, stderr)
	}
	aliceSnap, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("alice routing.Load: found=%v err=%v", found, err)
	}
	aliceAcct := auth.AccountKey(gateway.srv.URL, alice)
	if aliceSnap.Account != aliceAcct {
		t.Fatalf("alice Account = %q, want %q", aliceSnap.Account, aliceAcct)
	}

	// Bob signs in on the same profile: routing.Replace CAS swaps the owner to Bob.
	if out, stderr, err := desktopSetup(t, identityLine(bob, orgB().ID)+`{"code":"bob-code"}`+"\n", orgB().ID, teamB); err != nil {
		t.Fatalf("bob setup: %v\n%s%s", err, out, stderr)
	}
	bobSnap, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("bob routing.Load: found=%v err=%v", found, err)
	}
	bobAcct := auth.AccountKey(gateway.srv.URL, bob)
	if bobSnap.Account != bobAcct || bobSnap.StateID == aliceSnap.StateID {
		t.Fatalf("bob state = account:%q stateID:%q, want %q and fresh StateID", bobSnap.Account, bobSnap.StateID, bobAcct)
	}

	// Stale Alice writers (Update on ExpectConsent, Update on Expect, Delete on Expect) are all rejected.
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, aliceSnap.ExpectConsent(), func(s *routing.State) error {
		return s.Bind("github.com/acme/stale", orgA().ID, false)
	}); !errors.Is(err, routing.ErrAccountMismatch) {
		t.Fatalf("stale Alice Update(ExpectConsent) = %v, want ErrAccountMismatch", err)
	}
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, aliceSnap.Expect(), func(s *routing.State) error {
		return nil
	}); !errors.Is(err, routing.ErrAccountMismatch) {
		t.Fatalf("stale Alice Update(Expect) = %v, want ErrAccountMismatch", err)
	}
	if err := routing.Delete(testApp.stateDir, config.DefaultProfile, aliceSnap.Expect()); !errors.Is(err, routing.ErrAccountMismatch) {
		t.Fatalf("stale Alice Delete(Expect) = %v, want ErrAccountMismatch", err)
	}
	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || after.Account != bobAcct || len(after.Teams) != 1 || after.Teams[0].TeamID != teamB {
		t.Fatalf("Bob's state was altered after stale Alice writes: %+v %v %v", after, found, err)
	}
}

// Pre-activation destination-less spool events and old-layout outbox parts outside .teams/
// are dropped and counted under no_destination, never routed to Profile.Team.
func TestPreActivationDestinationlessSpoolAndOldOutboxDroppedAndCounted(t *testing.T) {
	f, ingest := pairingSandbox(t)
	acct := auth.AccountKey(f.srv.URL, "u-1")

	// 1. Pre-activation destination-less spool event (Enrollment == "", legacy AttrProjectID set).
	s := spoolForTest(t)
	if err := s.Append(spool.Event{
		Name: "terma.commit", Time: time.Now(), Repository: appRepo, Account: acct,
		Attrs: map[string]any{hookrun.AttrProjectID: acmeWeb},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := testApp.flushSpool(t.Context(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 0 || res.Unroutable != 1 || len(ingest.take()) != 0 {
		t.Fatalf("flush destination-less = %+v, want Sent=0 Unroutable=1", res)
	}
	spoolLosses, err := spool.ReadLosses(filepath.Join(testApp.stateDir, spool.Dir), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if spoolLosses.Counts["no_destination"] != 1 {
		t.Fatalf("spool losses = %v, want no_destination=1", spoolLosses.Counts)
	}

	// 2. Old-layout relay outbox part outside .teams/ is deleted and counted in losses.json.
	outboxDir := filepath.Join(claim.Dir(testApp.stateDir), relay.OutboxDir)
	legacyDir := filepath.Join(outboxDir, "@"+acct, acmeWeb, "codex", "_")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	partName := "01700000000000000000-000001-logs-3.pb"
	if err := os.WriteFile(filepath.Join(legacyDir, partName), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testApp.gateRoutingActivation(); err != nil {
		t.Fatalf("gateRoutingActivation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, partName)); !os.IsNotExist(err) {
		t.Fatalf("old-layout outbox part survived activation purge: err=%v", err)
	}
	relayLosses, err := relay.ReadOutboxLosses(outboxDir)
	if err != nil {
		t.Fatal(err)
	}
	if relayLosses.Counts["no_destination"] != 3 {
		t.Fatalf("relay outbox losses = %v, want no_destination=3", relayLosses.Counts)
	}
}

// Re-signing in via terma setup refreshes all sibling enrollments of the same account
// so SignInNeeded clears cleanly across every enrolled team.
func TestSignInRecoveryRefreshesAllEnrollments(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":2,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":2,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	gateway.grant("init-code", deskUser)
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID)+`{"code":"init-code"}`+"\n", orgA().ID, teamA); err != nil {
		t.Fatalf("setup teamA: %v\n%s%s", err, out, stderr)
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgB().ID), orgB().ID, teamB); err != nil {
		t.Fatalf("setup teamB: %v\n%s%s", err, out, stderr)
	}

	// Mark both enrollments SignInNeeded.
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(st.Teams) != 2 {
		t.Fatalf("pre-recovery state: found=%v len=%d err=%v", found, len(st.Teams), err)
	}
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, st.Expect(), func(s *routing.State) error {
		for _, tm := range s.Teams {
			if _, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Failure: routing.SignIn, At: time.Now()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Re-run setup for teamA: refreshes BOTH teamA and sibling teamB, clearing SignInNeeded on both.
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID), orgA().ID, teamA); err != nil {
		t.Fatalf("recovery setup: %v\n%s%s", err, out, stderr)
	}
	recovered, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(recovered.Teams) != 2 {
		t.Fatalf("post-recovery state: found=%v len=%d err=%v", found, len(recovered.Teams), err)
	}
	for _, tm := range recovered.Teams {
		if tm.SignInNeeded || tm.StateAt(time.Now()) != routing.Active {
			t.Fatalf("team %s still blocked after sign-in recovery: SignInNeeded=%v State=%s", tm.TeamID, tm.SignInNeeded, tm.StateAt(time.Now()))
		}
	}
}

// terma teardown removes routing/<profile>.json via CAS and cleans state directories.
func TestTeardownRemovesRoutingStateAndStateDirs(t *testing.T) {
	f, _ := pairingSandbox(t)
	_ = f
	hookruntest.RelayOn(t, testApp.stateDir)
	// Seed state directories that teardown must clean.
	dirs := []string{runs.Dir, hookrun.PushesDir, spool.Dir, config.PoliciesDir, claim.DirName}
	for _, d := range dirs {
		p := filepath.Join(testApp.stateDir, d)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "marker"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
		t.Fatalf("routing.State survived teardown: found=%v err=%v", found, err)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(testApp.stateDir, d)); !os.IsNotExist(err) {
			t.Fatalf("state dir %s survived teardown: err=%v", d, err)
		}
	}
}

// terma switch is removed as part of the PR 10 cutover.
func TestSwitchCommandIsRemoved(t *testing.T) {
	useConfigDir(t, t.TempDir())
	root := testApp.NewRootCommand()
	for _, sub := range root.Commands() {
		if sub.Name() == "switch" {
			t.Fatal("switch command is still registered on root")
		}
	}
	if _, err := runTerma(t, "switch"); err == nil {
		t.Fatal("terma switch succeeded, want unknown command error")
	}
}

func cloneTestApp() *App {
	cp := *testApp
	cp.flags = globalFlags{}
	cp.configure(testApp.dir, testApp.stateDir)
	return &cp
}

func runDesktopSetupApp(app *App, stdin, orgID, teamID string) (string, string, error) {
	cmd := app.NewRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"setup", "--desktop-grant", "-y", "--relay-service", "off",
		"--org", orgID, "--team", teamID,
	})
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// A delayed Alice setup blocked during policy fetch must fail closed and leave Bob's
// newly committed routing.State completely untouched.
func TestStaleAliceSetupCannotOverwriteBobRoutingState(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	const (
		alice = "11111111-1111-4111-8111-aaaaaaaaaaaa"
		bob   = "22222222-2222-4222-8222-bbbbbbbbbbbb"
	)
	gateway.grant("alice-race", alice)
	gateway.grant("bob-race", bob)

	aliceEntered := make(chan struct{})
	releaseAlice := make(chan struct{})
	gateway.onPolicy = func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.Header.Get("Authorization"), alice) {
			close(aliceEntered)
			<-releaseAlice
		}
		return false
	}

	aliceApp := cloneTestApp()
	aliceDone := make(chan error, 1)
	go func() {
		_, _, err := runDesktopSetupApp(aliceApp, identityLine(alice, orgA().ID)+`{"code":"alice-race"}`+"\n", orgA().ID, teamA)
		aliceDone <- err
	}()

	// Wait until Alice has signed in and is blocked inside /v1/policy, then complete Bob's setup.
	<-aliceEntered
	if out, stderr, err := desktopSetup(t, identityLine(bob, orgB().ID)+`{"code":"bob-race"}`+"\n", orgB().ID, teamB); err != nil {
		close(releaseAlice)
		t.Fatalf("bob setup: %v\n%s%s", err, out, stderr)
	}
	bobBefore, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		close(releaseAlice)
		t.Fatalf("bob routing.Load: found=%v err=%v", found, err)
	}

	close(releaseAlice)
	if err := <-aliceDone; err == nil {
		t.Fatal("stale Alice setup succeeded over Bob's routing state, want ErrAccountMismatch")
	}

	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	bobAcct := auth.AccountKey(gateway.srv.URL, bob)
	if err != nil || !found || after.Account != bobAcct || after.StateID != bobBefore.StateID || len(after.Teams) != 1 || after.Teams[0].TeamID != teamB {
		t.Fatalf("Bob's routing.State was modified by stale Alice setup: %+v (want StateID %s, Account %s)", after, bobBefore.StateID, bobAcct)
	}
}

// A setup target policy fetch that blocks while a concurrent transition advances the
// enrollment's epoch (e.g., SignInNeeded) must not clear the newer transition.
func TestSetupTargetFetchFencedByPreFetchEpoch(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	gateway.grant("epoch-init", deskUser)
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID)+`{"code":"epoch-init"}`+"\n", orgA().ID, teamA); err != nil {
		t.Fatalf("initial setup: %v\n%s%s", err, out, stderr)
	}
	initial, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(initial.Teams) != 1 {
		t.Fatalf("initial routing.Load: found=%v err=%v", found, err)
	}
	enrID := initial.Teams[0].EnrollmentID
	startEpoch := initial.Teams[0].Epoch

	// Advance teamA's epoch with SignIn while the second setup's policy fetch is in flight.
	entered := make(chan struct{})
	release := make(chan struct{})
	gateway.onPolicy = func(_ http.ResponseWriter, _ *http.Request) bool {
		close(entered)
		<-release
		return false
	}

	app2 := cloneTestApp()
	done := make(chan error, 1)
	go func() {
		_, _, err := runDesktopSetupApp(app2, identityLine(deskUser, orgA().ID), orgA().ID, teamA)
		done <- err
	}()

	<-entered
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, initial.Expect(), func(s *routing.State) error {
		_, err := s.ApplyRefresh(routing.Refresh{
			EnrollmentID: enrID,
			Epoch:        startEpoch,
			Failure:      routing.SignIn,
			At:           time.Now(),
		})
		return err
	}); err != nil {
		close(release)
		t.Fatalf("in-flight SignIn update: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("second setup: %v", err)
	}

	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(after.Teams) != 1 {
		t.Fatalf("after routing.Load: found=%v err=%v", found, err)
	}
	if !after.Teams[0].SignInNeeded || after.Teams[0].Epoch != startEpoch+1 {
		t.Fatalf("delayed setup cleared newer SignInNeeded transition: SignInNeeded=%v Epoch=%d (want true, %d)",
			after.Teams[0].SignInNeeded, after.Teams[0].Epoch, startEpoch+1)
	}
}

// Passing a Personal organization (Type: "personal") to --desktop-grant is rejected with
// GrantBadInput and writes neither credentials nor routing.State.
func TestDesktopGrantRejectsPersonalOrganization(t *testing.T) {
	gateway := grantSandbox(t)
	pers := organization{ID: "33333333-3333-4333-8333-333333333333", Name: "Personal", Role: "owner", Type: "personal"}
	gateway.orgs = []organization{orgA(), pers}
	t.Setenv("TERMA_POLICY_STUB", "")
	gateway.grant("pers-code", deskUser)

	stdin := identityLine(deskUser, pers.ID) + `{"code":"pers-code"}` + "\n"
	out, _, err := desktopSetup(t, stdin, pers.ID, "44444444-4444-4444-8444-444444444444")
	if err == nil {
		t.Fatalf("desktopSetup accepted personal org, output:\n%s", out)
	}
	if !strings.Contains(out, `"code":"`+auth.GrantBadInput+`"`) {
		t.Fatalf("stdout = %q, want GrantBadInput JSON", out)
	}
	if _, lerr := auth.LoadIdentity(testApp.dir, config.DefaultProfile); !errors.Is(lerr, auth.ErrNotLoggedIn) {
		t.Fatalf("rejected personal grant stored a sign-in: err=%v", lerr)
	}
	if _, found, lerr := routing.Load(testApp.stateDir, config.DefaultProfile); lerr != nil || found {
		t.Fatalf("rejected personal grant wrote routing.State: found=%v err=%v", found, lerr)
	}
}

// During sign-in recovery, a 403 ORG_NOT_ACCESSIBLE on one sibling enrollment refuses
// ALL sibling enrollments in that organization, even when another sibling returned 503.
func TestSetupRecoveryAppliesOrgAccessLostToAllSiblings(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA1 := projectsIn(orgA().ID)[0].ID
	teamA2 := projectsIn(orgA().ID)[1].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA1: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamA2: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/api"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB:  `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	gateway.grant("sib-init", deskUser)
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID)+`{"code":"sib-init"}`+"\n", orgA().ID, teamA1); err != nil {
		t.Fatalf("setup teamA1: %v\n%s%s", err, out, stderr)
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID), orgA().ID, teamA2); err != nil {
		t.Fatalf("setup teamA2: %v\n%s%s", err, out, stderr)
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgB().ID), orgB().ID, teamB); err != nil {
		t.Fatalf("setup teamB: %v\n%s%s", err, out, stderr)
	}

	// On recovery setup for orgB/teamB: orgA/teamA1 gets 403 ORG_NOT_ACCESSIBLE while
	// orgA/teamA2 gets 503 transient. Both orgA siblings must become Refused.
	gateway.onPolicy = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Query().Get("project_id") {
		case teamA1:
			orgNotAccessible(w)
			return true
		case teamA2:
			unavailable(w)
			return true
		}
		return false
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgB().ID), orgB().ID, teamB); err != nil {
		t.Fatalf("recovery setup teamB: %v\n%s%s", err, out, stderr)
	}

	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(after.Teams) != 3 {
		t.Fatalf("after routing.Load: found=%v len=%d err=%v", found, len(after.Teams), err)
	}
	now := time.Now()
	for _, tm := range after.Teams {
		switch tm.TeamID {
		case teamB:
			if tm.StateAt(now) != routing.Active {
				t.Fatalf("teamB state = %s, want Active", tm.StateAt(now))
			}
		case teamA1, teamA2:
			if tm.StateAt(now) != routing.Refused || tm.LastError != routing.OrgAccessLost {
				t.Fatalf("orgA sibling %s state=%s lastError=%q, want Refused / OrgAccessLost", tm.TeamID, tm.StateAt(now), tm.LastError)
			}
		}
	}
}

// A pre-routing relay that starts while setup's policy fetch is in flight (after the early
// check passed) still prevents setup from writing routing.State.
func TestActivationGateBlocksPreRoutingRelayStartedInFlight(t *testing.T) {
	origWait := daemon.GateStopWait
	daemon.GateStopWait = 120 * time.Millisecond
	t.Cleanup(func() { daemon.GateStopWait = origWait })

	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	team := projectsIn(orgA().ID)[0].ID
	key := "ter" + "_srv_11223344556677889900aabb"
	f.serverKeys = map[string]string{key: team}
	f.policyBody = `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`
	t.Setenv("TERMA_API_KEY", key)

	var unlock func()
	f.onPolicy = func(_ http.ResponseWriter, _ *http.Request) bool {
		dir := claim.Dir(testApp.stateDir)
		_ = os.MkdirAll(dir, 0o700)
		u, err := flock.TryLock(filepath.Join(dir, daemon.LockFile))
		if err == nil {
			unlock = u
			infoBytes, _ := json.Marshal(daemon.RunInfo{PID: os.Getpid(), Version: "0.9.0", Routing: false})
			_ = os.WriteFile(filepath.Join(dir, daemon.RunFile), infoBytes, 0o600)
		}
		return false
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()

	if _, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err == nil || !strings.Contains(err.Error(), "pre-routing relay") {
		t.Fatalf("setup with in-flight pre-routing relay = %v, want pre-routing relay error", err)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
		t.Fatalf("routing.State was written despite in-flight pre-routing relay: found=%v err=%v", found, err)
	}
	if gotKey, err := keystore.Get(testApp.dir, team); err == nil && gotKey != "" {
		t.Fatalf("server key was stored despite in-flight pre-routing relay: %q", gotKey)
	}
}

// Switching a server-key profile from team1 (orgA) to team2 (orgB) that lists the same
// repository computes fresh bindings for team2 rather than leaving the repo Pending.
func TestServerKeyTeamSwitchComputesFreshBindingsForSharedRepo(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	keyA := "ter" + "_srv_aaaa1111bbbb2222cccc3333"
	keyB := "ter" + "_srv_dddd4444eeee5555ffff6666"
	f.serverKeys = map[string]string{keyA: teamA, keyB: teamB}
	f.policyBody = `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`

	t.Setenv("TERMA_API_KEY", keyA)
	if out, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err != nil {
		t.Fatalf("server-key setup A: %v\n%s", err, out)
	}
	stA, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || stA.Bindings["github.com/acme/app"].Organization != orgA().ID {
		t.Fatalf("state after keyA = %+v found=%v err=%v", stA, found, err)
	}

	// Switch server key to teamB in orgB listing the exact same repo.
	t.Setenv("TERMA_API_KEY", keyB)
	if out, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err != nil {
		t.Fatalf("server-key setup B: %v\n%s", err, out)
	}
	stB, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load after keyB: found=%v err=%v", found, err)
	}
	if stB.Account != sendas.NoPersonLane || len(stB.Teams) != 1 || stB.Teams[0].TeamID != teamB {
		t.Fatalf("teams after keyB = %+v, want only %s", stB.Teams, teamB)
	}
	b, ok := stB.Bindings["github.com/acme/app"]
	if !ok || b.Organization != orgB().ID || b.Refused {
		t.Fatalf("Bindings after server-key team switch = %+v, want github.com/acme/app -> %s (not Pending)", stB.Bindings, orgB().ID)
	}
}

// When an enrolled target team returns 403 ORG_NOT_ACCESSIBLE during setup, setup records
// org-wide OrgAccessLost for the target and all its org siblings AND still recovers
// other-org enrollments from SignInNeeded before returning the error.
func TestSetupTargetOrgAccessLostRefusesAllOrgSiblingsAndRecoversOthers(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA1 := projectsIn(orgA().ID)[0].ID
	teamA2 := projectsIn(orgA().ID)[1].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA1: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamA2: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/api"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB:  `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	gateway.grant("target403-init", deskUser)
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID)+`{"code":"target403-init"}`+"\n", orgA().ID, teamA1); err != nil {
		t.Fatalf("setup teamA1: %v\n%s%s", err, out, stderr)
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgA().ID), orgA().ID, teamA2); err != nil {
		t.Fatalf("setup teamA2: %v\n%s%s", err, out, stderr)
	}
	if out, stderr, err := desktopSetup(t, identityLine(deskUser, orgB().ID), orgB().ID, teamB); err != nil {
		t.Fatalf("setup teamB: %v\n%s%s", err, out, stderr)
	}
	// Mark teamB SignInNeeded to prove other-org recovery still happens when target orgA returns 403.
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load: found=%v err=%v", found, err)
	}
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, st.Expect(), func(s *routing.State) error {
		for _, tm := range s.Teams {
			if tm.TeamID == teamB {
				_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: tm.EnrollmentID, Epoch: tm.Epoch, Failure: routing.SignIn, At: time.Now()})
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Target orgA/teamA1 returns 403 ORG_NOT_ACCESSIBLE while sibling orgA/teamA2 returns 503.
	gateway.onPolicy = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Query().Get("project_id") {
		case teamA1:
			orgNotAccessible(w)
			return true
		case teamA2:
			unavailable(w)
			return true
		}
		return false
	}
	if _, _, err := desktopSetup(t, identityLine(deskUser, orgA().ID), orgA().ID, teamA1); err == nil {
		t.Fatal("setup targeting 403 ORG_NOT_ACCESSIBLE team succeeded, want error")
	}

	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || len(after.Teams) != 3 {
		t.Fatalf("after routing.Load: found=%v len=%d err=%v", found, len(after.Teams), err)
	}
	now := time.Now()
	for _, tm := range after.Teams {
		switch tm.TeamID {
		case teamA1, teamA2:
			if tm.StateAt(now) != routing.Refused || tm.LastError != routing.OrgAccessLost {
				t.Fatalf("orgA team %s state=%s lastError=%q, want Refused/OrgAccessLost", tm.TeamID, tm.StateAt(now), tm.LastError)
			}
		case teamB:
			if tm.SignInNeeded || tm.StateAt(now) != routing.Active {
				t.Fatalf("orgB teamB SignInNeeded=%v state=%s, want false/Active", tm.SignInNeeded, tm.StateAt(now))
			}
		}
	}
}

// If a server-key setup snapshots teamA (skipping already-bound repos) and another setup
// switches the profile to teamB before the first commits, the stale setup is rejected so
// teamB is untouched and no repo is ever left Pending.
func TestConcurrentServerKeySetupRejectsStaleReplacement(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	keyA := "ter" + "_srv_1111aaaa2222bbbb3333cccc"
	keyB := "ter" + "_srv_4444dddd5555eeee6666ffff"
	f.serverKeys = map[string]string{keyA: teamA, keyB: teamB}
	f.policyBody = `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`

	t.Setenv("TERMA_API_KEY", keyA)
	if out, err := runTerma(t, "setup", "-y", "--relay-service", "off"); err != nil {
		t.Fatalf("initial server-key setup A: %v\n%s", err, out)
	}

	// Setup A re-runs for teamA and blocks in /v1/policy while Setup B switches to teamB.
	entered := make(chan struct{})
	release := make(chan struct{})
	f.onPolicy = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer "+keyA {
			close(entered)
			<-release
		}
		return false
	}

	appA := cloneTestApp()
	doneA := make(chan error, 1)
	go func() {
		cmd := appA.NewRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"setup", "-y", "--relay-service", "off"})
		doneA <- cmd.Execute()
	}()

	<-entered
	// Switch profile to keyB via a separate command with keyB in config override / env:
	// since env is process-wide, directly run server-key commit for teamB via routing.Replace
	// (simulating concurrent server-key setup B completing while A is blocked in FetchWithKey).
	stA, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		close(release)
		t.Fatalf("routing.Load A: found=%v err=%v", found, err)
	}
	polB := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/app"},
		TeamID: teamB, OrganizationID: orgB().ID, AuthURL: f.srv.URL, Revision: 1, FetchedAt: time.Now()}
	expA := stA.Expect()
	stB, err := routing.Replace(testApp.stateDir, config.DefaultProfile, &expA, sendas.NoPersonLane, func(s *routing.State) error {
		if _, err := s.Select(routing.Team{OrganizationID: orgB().ID, AuthURL: f.srv.URL, TeamID: teamB, Policy: polB}); err != nil {
			return err
		}
		return s.Bind("github.com/acme/app", orgB().ID, false)
	})
	if err != nil {
		close(release)
		t.Fatalf("concurrent setup B replace: %v", err)
	}

	close(release)
	if err := <-doneA; !errors.Is(err, routing.ErrStaleGeneration) {
		t.Fatalf("stale server-key setup A = %v, want ErrStaleGeneration", err)
	}

	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || after.StateID != stB.StateID || len(after.Teams) != 1 || after.Teams[0].TeamID != teamB || after.Bindings["github.com/acme/app"].Organization != orgB().ID {
		t.Fatalf("concurrent teamB state altered by stale setup A: %+v", after)
	}
}

// Live-account validation runs under routing/<profile>.lock: if Bob signs back in while
// Alice waits for the routing lock to replace Bob's unchanged StateID, Alice's Replace
// aborts under the lock and leaves Bob's routing.State untouched.
func TestLiveAccountCheckUnderRoutingLockPreventsStaleReplace(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	teamA := projectsIn(orgA().ID)[0].ID
	teamB := projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		teamB: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	const (
		alice = "11111111-1111-4111-8111-aaaaaaaaaaaa"
		bob   = "22222222-2222-4222-8222-bbbbbbbbbbbb"
	)
	gateway.grant("bob-first", bob)
	if out, stderr, err := desktopSetup(t, identityLine(bob, orgB().ID)+`{"code":"bob-first"}`+"\n", orgB().ID, teamB); err != nil {
		t.Fatalf("bob initial setup: %v\n%s%s", err, out, stderr)
	}
	bobState, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("bob routing.Load: found=%v err=%v", found, err)
	}
	aliceAcct := auth.AccountKey(gateway.srv.URL, alice)
	bobAcct := auth.AccountKey(gateway.srv.URL, bob)

	// Alice signs in and snapshots Bob's routing state as baseline.
	if err := saveSignIn(testApp.dir, storedSessionOf(gateway, alice)); err != nil {
		t.Fatal(err)
	}
	baseline := testApp.loadSetupBaseline(config.DefaultProfile, aliceAcct)
	target := routing.Team{
		OrganizationID: orgA().ID,
		AuthURL:        gateway.srv.URL,
		TeamID:         teamA,
		Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: []string{"github.com/acme/app"},
			TeamID: teamA, OrganizationID: orgA().ID, AuthURL: gateway.srv.URL, Revision: 1, FetchedAt: time.Now(),
		},
	}

	// Hold routing/<profile>.json.lock so Alice's Replace blocks waiting on the lock after
	// its pre-lock check passes, then switch live sign-in back to Bob before releasing lock!
	statePath, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	unlockRouting, err := flock.TryLock(statePath + ".lock")
	if err != nil {
		t.Fatalf("TryLock routing sidecar: %v", err)
	}
	aliceDone := make(chan error, 1)
	go func() {
		_, _, cerr := cloneTestApp().commitRoutingStateLocked(config.DefaultProfile, aliceAcct, baseline, target, nil, nil, false)
		aliceDone <- cerr
	}()

	// Give Alice a moment to pass the pre-lock check and block on flock.Locked, then sign Bob back in.
	time.Sleep(30 * time.Millisecond)
	if err := saveSignIn(testApp.dir, storedSessionOf(gateway, bob)); err != nil {
		unlockRouting()
		t.Fatal(err)
	}
	unlockRouting()

	if err := <-aliceDone; !errors.Is(err, routing.ErrAccountMismatch) {
		t.Fatalf("stale Alice Replace after Bob re-signed in = %v, want ErrAccountMismatch", err)
	}
	after, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found || after.Account != bobAcct || after.StateID != bobState.StateID {
		t.Fatalf("Bob's routing.State was overwritten while Bob was active: %+v", after)
	}
}

func TestDecideSetupBindingsWildcardPrompt(t *testing.T) {
	var captured []prompt.Item
	choose := func(_ string, items []prompt.Item) (int, error) {
		captured = slices.Clone(items)
		return 0, nil
	}

	t.Run("pure wildcard shows owner key directly", func(t *testing.T) {
		captured = nil
		target := routing.Team{
			OrganizationID: "org_acme",
			Policy: config.Policy{
				Mode:         config.ModeRepo,
				Wildcards:    true,
				Repositories: []string{"github.com/acme/*"},
			},
		}
		choices, err := decideSetupBindingsWith(target, "Acme", nil, true, choose)
		if err != nil {
			t.Fatalf("decideSetupBindings: %v", err)
		}
		if len(captured) == 0 || captured[0].Label != "Bind github.com/acme to Acme" {
			t.Fatalf("prompt label = %q, want %q", captured[0].Label, "Bind github.com/acme to Acme")
		}
		if len(choices) != 1 || choices[0].key != "github.com/acme" || choices[0].org != "org_acme" {
			t.Fatalf("choices = %+v, want [{key:github.com/acme org:org_acme}]", choices)
		}
	})

	t.Run("mixed wildcard and exact lists exact binding scope", func(t *testing.T) {
		captured = nil
		target := routing.Team{
			OrganizationID: "org_acme",
			Policy: config.Policy{
				Mode:         config.ModeRepo,
				Wildcards:    true,
				Repositories: []string{"github.com/acme/*", "github.com/other/repo"},
			},
		}
		choices, err := decideSetupBindingsWith(target, "Acme", nil, true, choose)
		if err != nil {
			t.Fatalf("decideSetupBindings: %v", err)
		}
		wantLabel := "Bind github.com/acme, github.com/other/repo to Acme"
		if len(captured) == 0 || captured[0].Label != wantLabel {
			t.Fatalf("prompt label = %q, want %q", captured[0].Label, wantLabel)
		}
		if len(choices) != 2 || choices[0].key != "github.com/acme" || choices[1].key != "github.com/other/repo" {
			t.Fatalf("choices = %+v, want [github.com/acme github.com/other/repo]", choices)
		}
	})
}

func TestTeardownRemovesRoutingStateForAllProfiles(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	signInTo(t, gateway, orgA())

	acct := auth.AccountKey(gateway.srv.URL, "user-1")
	for _, profile := range []string{config.DefaultProfile, "work"} {
		if _, err := routing.Create(testApp.stateDir, profile, acct, func(s *routing.State) error {
			_, err := s.Select(routing.Team{
				OrganizationID: orgA().ID,
				AuthURL:        gateway.srv.URL,
				TeamID:         "team-" + profile,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{"github.com/acme/" + profile},
					TeamID: "team-" + profile, OrganizationID: orgA().ID, AuthURL: gateway.srv.URL,
					Revision: 1, FetchedAt: time.Now(),
				},
			})
			return err
		}); err != nil {
			t.Fatalf("routing.Create(%s): %v", profile, err)
		}
	}
	// Also place a corrupt routing state under an unconfigured/stale profile name.
	corruptPath, err := routing.StatePath(testApp.stateDir, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	for _, profile := range []string{config.DefaultProfile, "work", "staging"} {
		if st, found, err := routing.Load(testApp.stateDir, profile); err != nil || found {
			t.Fatalf("profile %q still has routing state after teardown: found=%v err=%v state=%+v", profile, found, err, st)
		}
	}
}
