package cli

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("account = %q, want u-1's", got)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("cached account = %q, want u-1's", got)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, "u-2")); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
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
	if got := src.Account(); got != auth.AccountUnreadable {
		t.Fatalf("with the profile switched mid-read, account = %q; want it read again", got)
	}
	if got := src.Account(); got != sendas.NoPersonLane {
		t.Fatalf("read again, account = %q; want no person's lane", got)
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

// A retired sign-in is no credential doctor or status may call "your sign-in", and what
// they say fixes it is a newer terma, signed in again.
func TestARetiredSignInSendsNothingDoctorCounts(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, "u-2")); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
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

// On a server-key profile the relay's first look records the key's decision: what an older
// terma queued is no person's.
func TestTheRelaysFirstLookRecordsTheKeysDecision(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "organization_id": "o", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	serverKeyProfile(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	var engine relay.Options
	testApp.bindRelay(&engine, cfg)
	if got := engine.LegacyOwner(); got != auth.LegacyNoPerson {
		t.Fatalf("decision = %q; want no person's", got)
	}
	rec, err := auth.ReadLegacyRecord(testApp.stateDir)
	if err != nil || rec.Account != auth.LegacyNoPerson || !slices.Equal(rec.SessionIDs, []string{"s"}) {
		t.Fatalf("record = %+v, %v; want the key's decision, with the session", rec, err)
	}
}

// A config.json that can't be read never stops the relay's legacy look: no record is
// written, and the relay goes on.
func TestAnUnreadableConfigLeavesTheLegacyLookAlone(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(testApp.dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var engine relay.Options
	testApp.bindRelay(&engine, cfg)
	if got := engine.LegacyOwner(); got != "" {
		t.Fatalf("decision = %q; want none", got)
	}
	if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || !rec.RetiredAt.IsZero() {
		t.Fatalf("record = %+v, %v; want none written", rec, err)
	}
}

// A retirement is recorded on the session that sent the refused token even after another
// process rotated that session's token: it is matched by session, in user-credentials.json.
func TestARetirementIsRecordedAfterAnotherProcessRotated(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, cred); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, cred); err != nil {
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
