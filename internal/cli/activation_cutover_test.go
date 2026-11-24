package cli

import (
	"encoding/json"
	"errors"
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
