package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func testHeartbeatOpts(t *testing.T) relay.Options {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return testApp.relayDeps().Engine(t.Context(), testApp.stateDir, cfg, daemon.Settings{}, nil)
}

func emptyBeat() *logspb.LogsData {
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}
}

// On a person's profile a beat goes with the sign-in's token to the selected team's ingest,
// naming the team; it names the signed-in user only for the team whose validated policy the
// profile fetched, at the profile's own ingest. With no team selected nothing is sent, and
// nothing goes with a team key held here.
func TestHeartbeatGoesWithTheSignIn(t *testing.T) {
	var sent []string // "<bearer> <team>=<terma.user.id>" per beat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var beat logspb.LogsData
		if err := proto.Unmarshal(body, &beat); err != nil {
			t.Error(err)
		}
		user := ""
		for _, kv := range beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
			if kv.Key == semconv.TermaUserIDKey {
				user = kv.Value.GetStringValue()
			}
		}
		sent = append(sent, r.Header.Get("Authorization")+" "+r.Header.Get(relay.ProjectHeader)+"="+user)
	}))
	defer srv.Close()
	f := &fakeAuth{srv: srv}
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(testApp.dir, "team", "ter_srv_team", keystore.Hosts{OTLP: srv.URL}); err != nil {
		t.Fatal(err)
	}
	send := testHeartbeatOpts(t).HeartbeatSend
	if err := send(context.Background(), emptyBeat()); err == nil {
		t.Fatal("a beat with no team selected was sent")
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	if err := send(context.Background(), emptyBeat()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer " + fakeUserToken("u-1") + " team=u-1"}
	if strings.Join(sent, " ") != strings.Join(want, " ") {
		t.Fatalf("beats sent %q, want %q", sent, want)
	}
	if err := auth.DeleteCredential(testApp.dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if err := send(context.Background(), emptyBeat()); err == nil || len(sent) != 1 {
		t.Fatalf("a signed-out profile sent a beat: %v, %q", err, sent)
	}
}

// On a profile set up with a server key, a beat goes with the selected team's key, else
// another key held here, and names no one: a key is no person's.
func TestServerKeyHeartbeatNamesNoOne(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+" "+r.Header.Get(relay.ProjectHeader))
	}))
	defer srv.Close()
	authSandbox(t, &fakeAuth{srv: srv})
	serverKeyProfile(t)
	hosts := keystore.Hosts{OTLP: srv.URL, API: srv.URL}
	if err := keystore.Set(testApp.dir, "elsewhere", "ter_srv_elsewhere", hosts); err != nil {
		t.Fatal(err)
	}
	send := testHeartbeatOpts(t).HeartbeatSend
	if err := send(context.Background(), emptyBeat()); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(testApp.dir, "team", "ter_srv_team", hosts); err != nil {
		t.Fatal(err)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	if err := send(context.Background(), emptyBeat()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ter_srv_elsewhere ", "ter_srv_team "}; strings.Join(sent, "|") != strings.Join(want, "|") {
		t.Fatalf("beats sent %q, want %q", sent, want)
	}
}

// signedInUser names the profile's login from user-credentials.json: no one signed out,
// signed in for another environment, or set up with a server key.
func TestSignedInUser(t *testing.T) {
	useConfigDir(t, t.TempDir())
	cfg := config.Config{Dir: testApp.dir, ProfileName: config.DefaultProfile, AuthURL: "https://auth.example", OrganizationID: "org-a"}
	if got := signedInUser(&cfg); got != "" {
		t.Fatalf("signed out, named %q", got)
	}
	if _, err := auth.SaveCredential(testApp.dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_a", SessionID: "s-a", AuthURL: cfg.AuthURL, UserID: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if got := signedInUser(&cfg); got != "user-a" {
		t.Fatalf("signed in, named %q, want user-a", got)
	}
	for name, c := range map[string]config.Config{
		"server key":          {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: cfg.AuthURL, OrganizationID: "org-a", ServerKeySignIn: true},
		"another environment": {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: "https://auth.other", OrganizationID: "org-a"},
	} {
		if got := signedInUser(&c); got != "" {
			t.Errorf("%s: named %q", name, got)
		}
	}
}

// A relayed part names the signed-in user under the rule a heartbeat names them by: only one
// bound for the selected team's validated policy at this profile's ingest, under the
// configuration as it is when the relay asks, not as it started.
func TestRelayUserFollowsTheHeartbeatsRule(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	authSandbox(t, &fakeAuth{srv: srv})
	if err := saveSignIn(testApp.dir, &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
		ExpiresAt: time.Now().Add(time.Hour), AuthURL: srv.URL, SessionID: "s-" + "org-a", UserID: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization("org-a", "A") }); err != nil {
		t.Fatal(err)
	}
	relay := &config.Config{ProfileName: config.DefaultProfile}
	if got := testApp.relayUser(relay, "team", srv.URL); got != "" {
		t.Fatalf("with no team policy fetched, named %q", got)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	if got := testApp.relayUser(relay, "team", srv.URL); got != "user-a" {
		t.Fatalf("bound for the team, named %q, want user-a", got)
	}
	for name, to := range map[string][2]string{
		"another team":   {"elsewhere", srv.URL},
		"another ingest": {"team", "https://otel.other.example"},
	} {
		if got := testApp.relayUser(relay, to[0], to[1]); got != "" {
			t.Errorf("%s: named %q", name, got)
		}
	}
	// The relay asks about its own profile, whichever profile is active now.
	t.Setenv("TERMA_PROFILE", "other")
	if got := testApp.relayUser(relay, "team", srv.URL); got != "user-a" {
		t.Errorf("with another profile active, named %q, want the relay's own profile's user-a", got)
	}
	if got := testApp.relayUser(&config.Config{ProfileName: "other"}, "team", srv.URL); got != "" {
		t.Errorf("for a profile with no login, named %q", got)
	}
	t.Setenv("TERMA_PROFILE", "")
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeySignIn = true }); err != nil {
		t.Fatal(err)
	}
	if got := testApp.relayUser(relay, "team", srv.URL); got != "" {
		t.Errorf("set up with a server key, named %q", got)
	}
}

// A heartbeat refused as SIGN_IN_RETIRED marks the sign-in, so an idle machine's doctor says
// what fixes it, whatever status the refusal comes with: a 401 is not taken for an expired token.
func TestARetiredHeartbeatMarksTheSignIn(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"code":7,"message":"retired","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`)
			}))
			defer srv.Close()
			f := &fakeAuth{srv: srv}
			authSandbox(t, f)
			if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
				t.Fatal(err)
			}
			selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
			send := testHeartbeatOpts(t).HeartbeatSend
			if err := send(context.Background(), emptyBeat()); err == nil {
				t.Fatal("a refused beat was taken")
			}
			if !auth.Retired(testApp.dir, config.DefaultProfile) {
				t.Fatal("the retired sign-in was not marked")
			}
		})
	}
}

// A heartbeat carries the person's token, so it follows no redirect: Go would keep the
// Authorization header on a same-host https→http downgrade.
func TestAHeartbeatFollowsNoRedirect(t *testing.T) {
	var followed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	if err := sendHeartbeat(t.Context(), srv.URL, "user-token", "team", &logspb.LogsData{}); err == nil {
		t.Fatal("a redirected heartbeat counted as sent")
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("the heartbeat followed a redirect %d times", n)
	}
}

// What a person's token sends names that token's own user only: asked for another account
// (one signed in, or out, since the token was had), it names no one.
func TestAccountUserNamesOnlyTheTokensOwnUser(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	authSandbox(t, &fakeAuth{srv: srv})
	if err := saveSignIn(testApp.dir, &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
		ExpiresAt: time.Now().Add(time.Hour), AuthURL: srv.URL, SessionID: "s-" + "org-a", UserID: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization("org-a", "A") }); err != nil {
		t.Fatal(err)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for account, want := range map[string]string{
		auth.AccountKey(cfg.AuthURL, "user-a"): "user-a",
		auth.AccountKey(cfg.AuthURL, "user-b"): "",
		"":                                     "",
	} {
		if got := testApp.accountUser(cfg, "team", cfg.OTLPURL, account); got != want {
			t.Errorf("for account %q named %q, want %q", account, got, want)
		}
	}
}

// Heartbeats share the relay's sign-in source, so the system keychain is read once across
// many beats and OTLP sends for the same session rather than prompting on every beat.
func TestHeartbeatSharesTheRelaysSignInAndReadsTheKeychainOnce(t *testing.T) {
	f, ingest := pairingSandbox(t)
	if auth.StoredInFile(testApp.dir, config.DefaultProfile) {
		t.Fatal("test sign-in was stored in a file rather than the keychain; the test proves nothing")
	}
	reads := secret.ReadsForTest(t, testApp.dir)
	opts := testHeartbeatOpts(t)
	for i := range 5 {
		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); err != nil {
			t.Fatalf("beat %d: %v", i+1, err)
		}
	}
	token, err := opts.SignIn.Token(t.Context(), auth.AccountKey(f.srv.URL, "u-1"))
	if err != nil || token != fakeUserToken("u-1") {
		t.Fatalf("SignIn.Token = %q, %v", token, err)
	}
	if got := reads(); got != 1 {
		t.Fatalf("keychain read %d times across 5 heartbeats and a relay token lookup; want 1", got)
	}
	sent := ingest.take()
	if len(sent) != 5 {
		t.Fatalf("sent %d beats, want 5", len(sent))
	}
	wantAuth := "Bearer " + fakeUserToken("u-1")
	for i, r := range sent {
		if r.auth != wantAuth || r.project != acmeWeb {
			t.Fatalf("beat %d went with %q to %q; want %q to %q", i+1, r.auth, r.project, wantAuth, acmeWeb)
		}
	}
	// A locked keychain after the first read still lets later beats reuse the cached session.
	secret.FailForTest(t, testApp.dir)
	if err := opts.HeartbeatSend(t.Context(), emptyBeat()); err != nil {
		t.Fatalf("beat with locked keychain after cache warm: %v", err)
	}
	if got := reads(); got != 1 {
		t.Fatalf("keychain read %d times after locked-keychain beat; want 1", got)
	}
	if locked := ingest.take(); len(locked) != 1 || locked[0].auth != wantAuth || locked[0].project != acmeWeb {
		t.Fatalf("locked-keychain beat sent %+v; want %q to %q", locked, wantAuth, acmeWeb)
	}
}

// A heartbeat reads the profile's team and sign-in as one snapshot: an account switch
// between the two reads, or an unpaired sign-in stored before the profile is rewritten,
// sends no heartbeat and never pairs B's token with A's team.
func TestHeartbeatNeverPairsAnothersTokenWithTheLastTeam(t *testing.T) {
	tokenB := "Bearer " + fakeUserToken("u-2")
	betaCore := projectsIn(orgB().ID)[0].ID

	t.Run("switch between reads", func(t *testing.T) {
		f, ingest := pairingSandbox(t)
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		opts := testApp.relayDeps().Engine(t.Context(), testApp.stateDir, cfg, daemon.Settings{}, nil)
		switched := false
		testApp.betweenProfileReads = func() {
			if switched {
				return
			}
			switched = true
			if err := testApp.storeLogin(context.Background(), cfg, storedSessionOf(f, "u-2")); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(func() { testApp.betweenProfileReads = nil })

		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); !errors.Is(err, relay.ErrNoKey) {
			t.Fatalf("beat across account switch = %v, want %v", err, relay.ErrNoKey)
		}
		if sent := ingest.take(); len(sent) != 0 {
			t.Fatalf("beat across account switch sent %+v; want nothing", sent)
		}
		// After the switch completes and before B picks a team, no beat goes either.
		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); !errors.Is(err, relay.ErrNoKey) {
			t.Fatalf("beat after switch with no team = %v, want %v", err, relay.ErrNoKey)
		}
		if sent := ingest.take(); len(sent) != 0 {
			t.Fatalf("beat after switch with no team sent %+v; want nothing", sent)
		}

		// Once B chooses their own organization and team, heartbeats go with B's token to B's team.
		if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
			p.SelectOrganization(orgB().ID, orgB().Name)
		}); err != nil {
			t.Fatal(err)
		}
		selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: betaCore, OrganizationID: orgB().ID, AuthURL: f.srv.URL, Revision: 1, FetchedAt: time.Now()})
		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); err != nil {
			t.Fatalf("beat under B's own team: %v", err)
		}
		sent := ingest.take()
		if len(sent) != 1 || sent[0].auth != tokenB || sent[0].project != betaCore {
			t.Fatalf("B's beat sent %+v; want %q to %q", sent, tokenB, betaCore)
		}
	})

	t.Run("unpaired sign-in stored before profile", func(t *testing.T) {
		f, ingest := pairingSandbox(t)
		opts := testHeartbeatOpts(t)
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, "u-2")); err != nil {
			t.Fatal(err)
		}
		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); !errors.Is(err, relay.ErrNoKey) {
			t.Fatalf("beat with unpaired B credential = %v, want %v", err, relay.ErrNoKey)
		}
		if sent := ingest.take(); len(sent) != 0 {
			t.Fatalf("beat with unpaired B credential sent %+v; want nothing", sent)
		}
	})

	t.Run("switch to server key between reads", func(t *testing.T) {
		_, ingest := pairingSandbox(t)
		opts := testHeartbeatOpts(t)
		switched := false
		testApp.betweenProfileReads = func() {
			if switched {
				return
			}
			switched = true
			serverKeyProfile(t)
		}
		t.Cleanup(func() { testApp.betweenProfileReads = nil })

		if err := opts.HeartbeatSend(t.Context(), emptyBeat()); !errors.Is(err, relay.ErrNoKey) {
			t.Fatalf("beat across switch to server key = %v, want %v", err, relay.ErrNoKey)
		}
		if sent := ingest.take(); len(sent) != 0 {
			t.Fatalf("beat across switch to server key sent %+v; want nothing", sent)
		}
	})
}
