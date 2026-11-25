package cli

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The relay and the spool's delivery send as the sign-in in place, read again at each send:
// its own account gets its token; another account, a signed-out profile, a retired sign-in
// and a server-key profile get none, which holds what waits for them.
func TestTheSignInSourceSendsOnlyAsItsOwnAccount(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	if src.Account() != "" {
		t.Fatal("a signed-out profile has an account")
	}
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	src = testApp.signInSource(cfg)
	mine := auth.AccountKey(f.srv.URL, "u-1")
	if got := src.Account(); got != mine {
		t.Fatalf("account = %q, want u-1's", got)
	}
	if token, err := src.Token(t.Context(), mine); err != nil || token != fakeUserToken("u-1") {
		t.Fatalf("token = %q, %v", token, err)
	}
	if token, err := src.Token(t.Context(), auth.AccountKey(f.srv.URL, "u-2")); err != nil || token != "" {
		t.Fatalf("another account's token = %q, %v", token, err)
	}
	if err := auth.MarkRetired(testApp.dir, config.DefaultProfile, ""); err != nil {
		t.Fatal(err)
	}
	if token, err := src.Token(t.Context(), mine); err != nil || token != "" {
		t.Fatalf("a retired sign-in's token = %q, %v", token, err)
	}
	serverKeyProfile(t)
	if got := testApp.signInSource(cfg).Account(); got != sendas.NoPersonLane {
		t.Fatalf("a server-key profile has account %q, want no person's lane", got)
	}
}

// The account follows a sign-in another process changes at once, with no window in which
// a part arriving is bound to the account before: a switch to another person, a switch to
// a server key and a sign-out each show at the next part.
func TestTheSignInSourceFollowsASwitchAtOnce(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("account = %q, want u-1's", got)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("cached account = %q, want u-1's", got)
	}
	if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-2") {
		t.Fatalf("after a switch, account = %q, want u-2's", got)
	}
	serverKeyProfile(t)
	if got := src.Account(); got != sendas.NoPersonLane {
		t.Fatalf("after a switch to a server key, account = %q, want no person's lane", got)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeySignIn = false }); err != nil {
		t.Fatal(err)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-2") {
		t.Fatalf("back on the sign-in, account = %q, want u-2's", got)
	}
	if err := auth.DeleteCredential(testApp.dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if got := src.Account(); got != "" {
		t.Fatalf("after a sign-out, account = %q", got)
	}
}

// Account reads config.json and the sign-in as one snapshot: a switch to a server key that
// lands between the two reads is read again, never bound to the person signed in before.
func TestAccountReadsTheProfileAndTheSignInAsOne(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(testApp.dir, func(*config.File) {}); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	switched := false
	src.betweenReads = func() {
		if !switched {
			switched = true
			serverKeyProfile(t)
		}
	}
	if got := src.Account(); got != sendas.AccountUnreadable {
		t.Fatalf("with the profile switched mid-read, account = %q; want it read again", got)
	}
	if got := src.Account(); got != sendas.NoPersonLane {
		t.Fatalf("read again, account = %q; want no person's lane", got)
	}
}

// A hook, and a push reported after git push exits, read config.json and the sign-in as one
// snapshot: a sign-in as someone else that lands between the two reads makes the account
// unreadable, so the event is lost and counted, never stamped with the new person's account.
func TestAHookReadsTheProfileAndTheSignInAsOne(t *testing.T) {
	for _, path := range []struct {
		name string
		bind func() *spool.Spool
	}{
		{"hook", func() *spool.Spool {
			p := testApp.hookProfile()
			s := testApp.openSpool()
			s.Bind(p.Account) // as the hook's runtime does (spool.LaneOf)
			return s
		}},
		{"push", func() *spool.Spool { return testApp.pushEnv().Spool }},
	} {
		t.Run(path.name, func(t *testing.T) {
			f := newFakeAuth(t)
			authSandbox(t, f)
			if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-a")); err != nil {
				t.Fatal(err)
			}
			if err := config.UpdateFile(testApp.dir, func(*config.File) {}); err != nil {
				t.Fatal(err)
			}
			testApp.betweenProfileReads = func() {
				if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-b")); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { testApp.betweenProfileReads = nil })
			s := path.bind()
			if err := s.Append(spool.Event{Name: "a-event"}); err != nil {
				t.Fatal(err)
			}
			if got := queued(t, s); len(got) != 0 {
				t.Fatalf("with the sign-in replaced mid-read, queued %+v; want the event lost, not person b's", got)
			}
			losses, err := spool.ReadLosses(filepath.Join(testApp.stateDir, spool.Dir), time.Now())
			if err != nil || !maps.Equal(losses.Counts, map[string]int{sendas.LostUnreadable: 1}) {
				t.Fatalf("losses = %v, %v; want one unreadable", losses.Counts, err)
			}
		})
	}
}

// Doctor reads what the relay lost by reason from its outbox's losses.json.
func TestDoctorReadsTheRelaysLossesByReason(t *testing.T) {
	useConfigDir(t, t.TempDir())
	at := time.Now().UTC().Truncate(time.Second)
	losses := relay.OutboxLosses{Counts: map[string]int{"signed_out": 2, "unreadable": 1}, Last: at}
	if err := config.WriteJSON(filepath.Join(claim.Dir(testApp.stateDir), relay.OutboxDir, "losses.json"), losses, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := testApp.relayFacts(); !maps.Equal(got.OutboxLosses, losses.Counts) || !got.OutboxLossAt.Equal(at) {
		t.Fatalf("doctor read %v at %v; want %v at %v", got.OutboxLosses, got.OutboxLossAt, losses.Counts, at)
	}
}

// Doctor counts what hooks lost with what the relay lost, by reason, in one row, each
// reason in words.
func TestDoctorAddsTheHooksLossesToTheRelays(t *testing.T) {
	useConfigDir(t, t.TempDir())
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	losses := relay.OutboxLosses{Counts: map[string]int{"signed_out": 2, "outbox_full": 1, "policy_timeout": 4}, Last: at}
	if err := config.WriteJSON(filepath.Join(claim.Dir(testApp.stateDir), relay.OutboxDir, "losses.json"), losses, 0o600); err != nil {
		t.Fatal(err)
	}
	s := testApp.openSpool()
	if s.CountLost(sendas.LostSignedOut, 3) != nil || s.CountLost(sendas.LostUnreadable, 1) != nil {
		t.Fatal("could not count")
	}
	got := testApp.relayFacts()
	want := map[string]int{"signed_out": 5, "outbox_full": 1, "unreadable": 1, "policy_timeout": 4}
	if !maps.Equal(got.OutboxLosses, want) || !got.OutboxLossAt.After(at) {
		t.Fatalf("doctor read %v at %v; want %v after %v", got.OutboxLosses, got.OutboxLossAt, want, at)
	}
	c := doctor.OutboxCheck(got)
	if !strings.Contains(c.Detail, "5 collected while signed out") || !strings.Contains(c.Detail, "1 collected while the sign-in could not be read") ||
		!strings.Contains(c.Detail, "4 claimed for a team whose policy or key the relay couldn't read within 15 minutes") {
		t.Fatalf("check = %+v", c)
	}
}

// A retired sign-in is no credential doctor or status may call "your sign-in", and what
// they say fixes it is a newer terma, signed in again.
func TestARetiredSignInSendsNothingDoctorCounts(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := testApp.deliveryCredential(cfg)("", "p1"); got != "your sign-in" {
		t.Fatalf("a working sign-in reads %q", got)
	}
	if err := auth.MarkRetired(testApp.dir, config.DefaultProfile, ""); err != nil {
		t.Fatal(err)
	}
	if got := testApp.deliveryCredential(cfg)("", "p1"); got != "" {
		t.Fatalf("a retired sign-in reads %q, want none", got)
	}
	if got := testApp.signInFix(cfg); got != doctor.RetiredFix {
		t.Fatalf("fix = %q, want %q", got, doctor.RetiredFix)
	}
}

// A retirement marks the sign-in that sent the refused token, never one made since: another
// route's send may already hold the new sign-in's client.
func TestARetirementMarksOnlyTheSignInThatSent(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	mine := auth.AccountKey(f.srv.URL, "u-2")
	if token, err := src.Token(t.Context(), mine); err != nil || token == "" {
		t.Fatalf("token = %q, %v", token, err)
	}
	src.Retired(fakeUserToken("u-1")) // the old session's answer, arriving late
	if auth.Retired(testApp.dir, config.DefaultProfile) {
		t.Fatal("the new sign-in was retired for the old one's refusal")
	}
	src.Retired(fakeUserToken("u-2"))
	if !auth.Retired(testApp.dir, config.DefaultProfile) {
		t.Fatal("the sign-in that sent the refused token was not retired")
	}
}

// Account never waits on the client lock, which a keychain read may hold for a prompt.
func TestAccountDoesNotWaitOnTheClient(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	src.mu.Lock()
	defer src.mu.Unlock()
	done := make(chan struct{})
	go func() { src.Account(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Account waited on the client lock")
	}
}

// A refusal for a token the same session has rotated since still retires that session: two
// routes may race a send against another's refresh.
func TestARetirementCountsAfterTheSessionRotated(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	mine := auth.AccountKey(f.srv.URL, "u-1")
	sent, err := src.Token(t.Context(), mine)
	if err != nil || sent == "" {
		t.Fatalf("token = %q, %v", sent, err)
	}
	// The session sent an earlier token before another route's refresh rotated it.
	src.mu.Lock()
	session := src.client.Credential().SessionID
	src.mu.Unlock()
	src.handedOut("an-earlier-token", session)
	src.Retired("an-earlier-token")
	if !auth.Retired(testApp.dir, config.DefaultProfile) {
		t.Fatal("the session that sent the refused token was not retired after it rotated")
	}
}

// A retirement is recorded on the session that sent the refused token even after another
// process rotated that session's token: it is matched by session, in user-credentials.json.
func TestARetirementIsRecordedAfterAnotherProcessRotated(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	sent, err := src.Token(t.Context(), auth.AccountKey(f.srv.URL, "u-1"))
	if err != nil || sent == "" {
		t.Fatalf("token = %q, %v", sent, err)
	}
	// Another process rotates the same session's pair.
	rotated := storedSession(f)
	rotated.AccessToken, rotated.RefreshToken = rotated.AccessToken+"-2", rotated.RefreshToken+"-2"
	if err := auth.UpdateCredential(testApp.dir, config.DefaultProfile, rotated); err != nil {
		t.Fatal(err)
	}
	src.Retired(sent)
	if !auth.Retired(testApp.dir, config.DefaultProfile) {
		t.Fatal("the retirement was lost to a rotation in another process")
	}
}

// A sign-in for another environment is no credential doctor may count here, though what it
// collected keeps its account.
func TestASignInForAnotherEnvironmentIsNoDeliveryCredential(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cred := storedSession(f)
	cred.AuthURL = "https://auth.elsewhere.example.test"
	if err := saveSignIn(testApp.dir, cred); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := testApp.deliveryCredential(cfg)("", "p1"); got != "" {
		t.Fatalf("another environment's sign-in reads %q, want none", got)
	}
	if got := testApp.signInSource(cfg).Account(); got != auth.AccountKey(cred.AuthURL, "u-1") {
		t.Fatalf("its account = %q, want its own", got)
	}
}

// A stored sign-in with no user id is no delivery credential: nothing can be sent as it.
func TestASignInWithNoUserIsNoDeliveryCredential(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cred := storedSession(f)
	cred.UserID = ""
	if err := saveSignIn(testApp.dir, cred); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := testApp.deliveryCredential(cfg)("", "p1"); got != "" {
		t.Fatalf("a sign-in with no user reads %q, want none", got)
	}
}

// queued is the spool's events by name.
func queued(t *testing.T, s *spool.Spool) map[string]spool.Event {
	t.Helper()
	events, err := s.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]spool.Event{}
	for _, e := range events {
		out[e.Name] = e
	}
	return out
}

// A sign-in the profile is not paired with (an interrupted switch) is no delivery
// credential: the relay reads its account as unreadable and sends nothing with it (#166).
func TestAnUnpairedSignInIsNoDeliveryCredential(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.Account = auth.AccountKey(f.srv.URL, "u-2")
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := testApp.signInSource(cfg).Account(); got != sendas.AccountUnreadable {
		t.Fatalf("the relay reads the account as %q, want unreadable", got)
	}
	if got := testApp.deliveryCredential(cfg)("", "p1"); got != "" {
		t.Fatalf("an unpaired sign-in reads %q, want none", got)
	}
}
