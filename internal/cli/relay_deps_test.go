package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Only a beat that goes with the key of the team whose validated policy the login fetched names
// the signed-in user, unpinned too: a beat that falls back to another key held here, goes with a
// key TERMA_TEAM_ID alone picked, to another environment's ingest or with a key another
// deployment's API host filed, names no one, and so does one sent while the login is another
// organization's.
func TestHeartbeatNamesTheUserOnlyWithTheTeamKey(t *testing.T) {
	var sent []string // "<key>=<terma.user.id>" per beat the ingest accepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			http.Error(w, "refused", http.StatusForbidden) // the team's key cannot be minted
			return
		}
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
		sent = append(sent, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+"="+user)
	}))
	defer srv.Close()
	f := &fakeAuth{srv: srv}
	authSandbox(t, f)
	signIn := func(org, user string) {
		t.Helper()
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_" + org,
			RefreshToken: "ter_clr_" + org, ExpiresAt: time.Now().Add(time.Hour), AuthURL: srv.URL, OrganizationID: org, UserID: user}); err != nil {
			t.Fatal(err)
		}
	}
	signIn("org-a", "user-a")
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization("org-a", "A") }); err != nil {
		t.Fatal(err)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	hosts := keystore.Hosts{OTLP: srv.URL, API: srv.URL}
	if err := keystore.Set(testApp.dir, "elsewhere", "ter_srv_elsewhere", hosts); err != nil {
		t.Fatal(err)
	}
	beat := func() *logspb.LogsData {
		return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}
	}
	send := func() {
		t.Helper()
		if err := testApp.relayHeartbeatSend(context.Background(), beat()); err != nil {
			t.Fatal(err)
		}
	}
	send() // the team has no key and cannot be minted one: the beat falls back
	if err := keystore.Set(testApp.dir, "team", "ter_srv_team", hosts); err != nil {
		t.Fatal(err)
	}
	send()
	signIn("org-b", "user-b") // the login moved to another organization; the profile did not
	send()
	signIn("org-a", "user-a")
	// No team selected, so no validated policy: TERMA_TEAM_ID alone picks the key.
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Team = "" }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_TEAM_ID", "elsewhere")
	send()
	t.Setenv("TERMA_TEAM_ID", "")
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	// Unpinned, the profile's ingest is its environment's, while the team's key was filed with
	// this test ingest's hosts: another environment's, so the beat goes there naming no one.
	t.Setenv("TERMA_OTLP_URL", "")
	send()
	// The profile's own ingest is the one the team's key was filed with: named.
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OTLPURL = srv.URL }); err != nil {
		t.Fatal(err)
	}
	send()
	// Another deployment sharing that ingest, by its API host, filed the next team's key.
	if err := keystore.Set(testApp.dir, "team2", "ter_srv_team2", keystore.Hosts{OTLP: srv.URL, API: "https://api.other.example"}); err != nil {
		t.Fatal(err)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team2", Revision: 1, FetchedAt: time.Now()})
	send()
	want := []string{"ter_srv_elsewhere=", "ter_srv_team=user-a", "ter_srv_team=", "ter_srv_elsewhere=", "ter_srv_team=",
		"ter_srv_team=user-a", "ter_srv_team2="}
	if strings.Join(sent, " ") != strings.Join(want, " ") {
		t.Fatalf("beats sent %q, want %q", sent, want)
	}
}

// signedInUser names the profile's login from credentials.json: no one signed out, signed
// in for another organization or environment, or set up with a server key.
func TestSignedInUser(t *testing.T) {
	useConfigDir(t, t.TempDir())
	cfg := config.Config{Dir: testApp.dir, ProfileName: config.DefaultProfile, AuthURL: "https://auth.example", OrganizationID: "org-a"}
	if got := signedInUser(&cfg); got != "" {
		t.Fatalf("signed out, named %q", got)
	}
	if _, err := auth.SaveCredential(testApp.dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_a", OrganizationID: "org-a", AuthURL: cfg.AuthURL, UserID: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if got := signedInUser(&cfg); got != "user-a" {
		t.Fatalf("signed in, named %q, want user-a", got)
	}
	for name, c := range map[string]config.Config{
		"server key":          {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: cfg.AuthURL, OrganizationID: "org-a", ServerKeySignIn: true},
		"another environment": {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: "https://auth.other", OrganizationID: "org-a"},
		"another org":         {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: cfg.AuthURL, OrganizationID: "org-b"},
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
		ExpiresAt: time.Now().Add(time.Hour), AuthURL: srv.URL, OrganizationID: "org-a", UserID: "user-a"}); err != nil {
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
