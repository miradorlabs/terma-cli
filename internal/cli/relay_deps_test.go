package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(testApp.dir, "team", "ter_srv_team", keystore.Hosts{OTLP: srv.URL}); err != nil {
		t.Fatal(err)
	}
	beat := func() *logspb.LogsData {
		return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}
	}
	if err := testApp.relayHeartbeatSend(context.Background(), beat()); err == nil {
		t.Fatal("a beat with no team selected was sent")
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	if err := testApp.relayHeartbeatSend(context.Background(), beat()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer " + fakeUserToken("u-1") + " team=u-1"}
	if strings.Join(sent, " ") != strings.Join(want, " ") {
		t.Fatalf("beats sent %q, want %q", sent, want)
	}
	if err := auth.DeleteCredential(testApp.dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if err := testApp.relayHeartbeatSend(context.Background(), beat()); err == nil || len(sent) != 1 {
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
	beat := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}
	if err := testApp.relayHeartbeatSend(context.Background(), beat); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(testApp.dir, "team", "ter_srv_team", hosts); err != nil {
		t.Fatal(err)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	if err := testApp.relayHeartbeatSend(context.Background(), beat); err != nil {
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
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
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
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
				t.Fatal(err)
			}
			selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
			beat := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}
			if err := testApp.relayHeartbeatSend(context.Background(), beat); err == nil {
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

// A legacy record that can't be read is no decision, never a server key's: before one is
// read nothing is taken in, and after, a failed read keeps the last one.
func TestAnUnreadableLegacyRecordIsNoDecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auth.LegacyRecordFile)
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := cachedLegacyRecord(t.TempDir(), dir, func() (bool, error) { return true, nil })(); got.Account != "" {
		t.Fatalf("unreadable record = %+v; want no decision", got)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	want := auth.AccountKey("https://auth.example", "u")
	if err := auth.RecordLegacy(dir, []auth.LegacySignIn{{SessionID: "s", UserID: "u", AuthURL: "https://auth.example"}}, want, time.Now()); err != nil {
		t.Fatal(err)
	}
	legacy := cachedLegacyRecord(t.TempDir(), dir, func() (bool, error) { return false, nil })
	if good := legacy(); good.Account != want {
		t.Fatalf("record = %+v", good)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := legacy(); got.Account != want {
		t.Fatalf("after a failed read, record = %+v; want the last one read", got)
	}
}

// What a person's token sends names that token's own user only: asked for another account
// (one signed in, or out, since the token was had), it names no one.
func TestAccountUserNamesOnlyTheTokensOwnUser(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	authSandbox(t, &fakeAuth{srv: srv})
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
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

// While the profile's mode can't be read, there is no decision, and none is written: once
// it reads as a server key's, the key's decision is.
func TestALegacyLookupWithAnUnreadableProfileIsNoDecision(t *testing.T) {
	dir, stateDir := t.TempDir(), t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var readable atomic.Bool
	legacy := cachedLegacyRecord(dir, stateDir, func() (bool, error) {
		if !readable.Load() {
			return false, errors.New("permission denied")
		}
		return true, nil
	})
	if got := legacy(); got.Account != "" {
		t.Fatalf("with the profile unreadable: %+v; want no decision", got)
	}
	if rec, err := auth.ReadLegacyRecord(stateDir); err != nil || rec.Account != "" {
		t.Fatalf("record = %+v, %v; want none written", rec, err)
	}
	readable.Store(true)
	if got := legacy(); got.Account != auth.LegacyNoPerson {
		t.Fatalf("readable again, as a server key's: %+v; want no person's", got)
	}
}
