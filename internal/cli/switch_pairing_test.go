package cli

import (
	"context"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// pairingSandbox is u-1 (A) signed in with Acme and its team acmeWeb chosen under that
// sign-in, acmeWeb's policy stored, and every delivery going to ingest.
func pairingSandbox(t *testing.T) (*fakeAuth, *recordingIngest) {
	t.Helper()
	f := switchSandbox(t, orgA(), acmeWeb)
	ingest := newRecordingIngest(t)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, Repositories: []string{appRepo.Origin}, IncludePrompts: true, IncludeToolContent: true,
		TeamID: acmeWeb, OrganizationID: orgA().ID, AuthURL: f.srv.URL, Revision: 1, FetchedAt: time.Now()})
	return f, ingest
}

// hookAppend queues an event as a hook running now would: bound to the lane its profile
// reads, for the team that profile names.
func hookAppend(t *testing.T) dispatch.Profile {
	t.Helper()
	p := testApp.hookProfile()
	s := spoolForTest(t)
	s.Bind(p.Account)
	if err := s.Append(spool.Event{Name: "terma.commit", Time: time.Now(), Repository: appRepo, Attrs: map[string]any{hookrun.AttrProjectID: p.Team}}); err != nil {
		t.Fatal(err)
	}
	return p
}

// A switch from A to B whose revoke of A's session is held open sends nothing to A's team:
// a hook and the relay running during the wait never pair B's sign-in with the team chosen
// under A's, and nothing reaches the ingest naming that team, before or after the switch.
func TestASwitchHeldAtTheRevokeSendsNothingToTheLastTeam(t *testing.T) {
	f, ingest := pairingSandbox(t)
	b := storedSessionOf(f, "u-2")
	accountB := b.Account()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	f.revoking, f.holdRevokes = make(chan struct{}, 1), make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(f.holdRevokes)
		}
	}
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- testApp.storeLogin(context.Background(), cfg, b) }()
	select {
	case <-f.revoking:
	case err := <-done:
		t.Fatalf("the login finished without revoking A's session: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the login never revoked A's session")
	}

	// During the wait: the hook, and the relay A's sign-in started.
	if got := hookAppend(t); got.Account == accountB && got.Team == acmeWeb {
		t.Errorf("a hook during the revoke read B's sign-in with A's team %s", got.Team)
	}
	lane := testApp.signInSource(cfg).Account()
	if _, err := testApp.relayDeps().Resolver(cfg)(claim.Claim{ProjectID: acmeWeb, Repository: appRepo, Account: cfg.Account}); err == nil && lane == accountB {
		t.Error("the relay during the revoke binds parts to B's sign-in and resolves A's team")
	}

	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if org, team, _ := selected(t); org != "" || team != "" {
		t.Fatalf("after the switch to B: organization %q team %q, chosen under A", org, team)
	}
	if got := testApp.hookProfile(); got.Account != accountB || got.Team != "" {
		t.Fatalf("after the switch a hook reads %+v; want B's lane and no team", got)
	}
	hookAppend(t)
	if _, err := testApp.flushSpool(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range ingest.take() {
		if r.project == acmeWeb {
			t.Fatalf("a request reached the ingest for A's team %s with %q", acmeWeb, r.auth)
		}
	}
}

// B's sign-in stored over A's, with the profile not yet rewritten for it (storeLogin's
// moment between the two writes, or a crash there), is read by no one as B's: every reader
// of the pair gets the unreadable lane, so the hook's and the push's events are lost, the
// relay binds nothing, and a flush sends nothing.
func TestASignInStoredOverAnotherAccountsProfileIsNoLane(t *testing.T) {
	f, ingest := pairingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	if got := testApp.hookProfile(); got.Account != sendas.AccountUnreadable {
		t.Errorf("a hook reads lane %q with A's team %q; want the unreadable lane", got.Account, got.Team)
	}
	if lane := testApp.pushEnv().Spool.Lane(); lane.Account != "" || lane.Lost != sendas.LostUnreadable {
		t.Errorf("a push is bound to %+v; want it lost as unreadable", lane)
	}
	if lane := testApp.signInSource(cfg).Account(); lane != sendas.AccountUnreadable {
		t.Errorf("the relay binds parts to lane %q; want the unreadable lane", lane)
	}
	appendEvent(t, spoolForTest(t), acmeWeb, time.Now())
	if _, err := testApp.flushSpool(t.Context(), true, 0); err != nil {
		t.Fatal(err)
	}
	if sent := ingest.take(); len(sent) != 0 {
		t.Fatalf("a flush sent %d requests, the first for team %s with %q", len(sent), sent[0].project, sent[0].auth)
	}
}

// A relay started under A's sign-in resolves nothing once the profile is paired with B, even
// in the same organization: its team was chosen under A.
func TestTheRelayResolvesNothingOnceTheProfileIsAnotherAccounts(t *testing.T) {
	f, _ := pairingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolve := testApp.relayDeps().Resolver(cfg)
	if _, err := resolve(claim.Claim{ProjectID: acmeWeb, Repository: appRepo, Account: cfg.Account}); err != nil {
		t.Fatalf("under A's own sign-in: %v", err)
	}
	if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(claim.Claim{ProjectID: acmeWeb, Repository: appRepo, Account: cfg.Account}); err == nil {
		t.Fatal("a relay started under A resolved A's team for the profile paired with B")
	}
}

// An organization chosen under A's sign-in is not recorded once B's is stored, nor is a
// policy A's sign-in fetched recorded as the profile's team.
func TestAChoiceUnderOneSignInIsNotRecordedForAnother(t *testing.T) {
	f, _ := pairingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.chooseOrganization(context.Background(), cfg, storedSession(f), orgRef{ID: orgA().ID}, nil); err == nil {
		t.Error("an organization chosen under A's sign-in was recorded for B's")
	}
	_ = testApp.policies().Refresh(t.Context(), cfg) // fetched under A's sign-in, with A's organization
	if org, team, _ := selected(t); org != "" || team != "" {
		t.Fatalf("B's profile has organization %q team %q, chosen under A", org, team)
	}
}

// A login pairs the profile only with the sign-in stored: when another login's credential
// lands before the pairing, the profile keeps the pairing it had, and the login fails.
func TestALoginPairsTheProfileOnlyWithTheSignInStored(t *testing.T) {
	f, _ := pairingSandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// C's login stored its credential after B's, and has not paired the profile yet.
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, "u-3")); err != nil {
		t.Fatal(err)
	}
	if err := testApp.pairProfile(cfg, storedSessionOf(f, "u-2").Account()); err == nil {
		t.Error("B's login paired the profile over C's credential")
	}
	if got, _ := testApp.loadConfig(); got.Account != cfg.Account || got.Team != acmeWeb {
		t.Fatalf("the profile is paired with %q, team %q; want it as A left it", got.Account, got.Team)
	}
}

// storeAgain stores cred over whatever sign-in the profile holds, as a login does.
func storeAgain(t *testing.T, cred *auth.Credential) {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, cred); err != nil {
		t.Fatal(err)
	}
}

// Signing in again as the same person keeps the organization and team chosen under them.
func TestTheSamePersonSigningInAgainKeepsTheTeam(t *testing.T) {
	f, _ := pairingSandbox(t)
	again := storedSession(f)
	again.SessionID, again.RefreshToken = "s-again", refreshTokenPrefix+"again"
	storeAgain(t, again)
	if org, team, _ := selected(t); org != orgA().ID || team != acmeWeb {
		t.Fatalf("after signing in again: organization %q team %q", org, team)
	}
}

// A profile no sign-in was paired with (an earlier build's) loses its organization and team
// at the next sign-in, even the same person's: who chose them is unknown.
func TestAnUnpairedProfileIsClearedAtTheNextSignIn(t *testing.T) {
	f, _ := pairingSandbox(t)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Account = "" }); err != nil {
		t.Fatal(err)
	}
	again := storedSession(f)
	again.SessionID, again.RefreshToken = "s-again", refreshTokenPrefix+"again"
	storeAgain(t, again)
	if org, team, _ := selected(t); org != "" || team != "" {
		t.Fatalf("an unpaired profile kept organization %q team %q", org, team)
	}
}

// A profile set up only with a server key loses the key's team at a person's sign-in.
func TestAServerKeyOnlyProfileLosesItsTeamAtASignIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(orgA().ID, orgA().Name)
		p.Team, p.ServerKeySignIn, p.ServerKeyAuthURL = acmeWeb, true, f.srv.URL
	}); err != nil {
		t.Fatal(err)
	}
	storeAgain(t, storedSession(f))
	if org, team, _ := selected(t); org != "" || team != "" {
		t.Fatalf("a person signed in with the server key's organization %q team %q", org, team)
	}
}
