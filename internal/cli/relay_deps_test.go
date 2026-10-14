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
	"github.com/miradorlabs/terma-cli/internal/relay"
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

// signedInUser names the profile's login from credentials.json, by id and email: no one signed
// out, signed in for another organization or environment, set up with a server key, or with a
// credential that has not learnt its user's id, whose email alone names no one.
func TestSignedInUser(t *testing.T) {
	useConfigDir(t, t.TempDir())
	cfg := config.Config{Dir: testApp.dir, ProfileName: config.DefaultProfile, AuthURL: "https://auth.example", OrganizationID: "org-a"}
	if got := signedInUser(&cfg); got != (relay.User{}) {
		t.Fatalf("signed out, named %+v", got)
	}
	save := func(user string) {
		t.Helper()
		if _, err := auth.SaveCredential(testApp.dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_a", OrganizationID: "org-a",
			AuthURL: cfg.AuthURL, UserID: user, UserEmail: "a@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	save("")
	if got := signedInUser(&cfg); got != (relay.User{}) {
		t.Fatalf("signed in with no user id, named %+v", got)
	}
	save("user-a")
	if got, want := signedInUser(&cfg), (relay.User{ID: "user-a", Email: "a@example.com"}); got != want {
		t.Fatalf("signed in, named %+v, want %+v", got, want)
	}
	for name, c := range map[string]config.Config{
		"server key":          {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: cfg.AuthURL, OrganizationID: "org-a", ServerKeySignIn: true},
		"another environment": {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: "https://auth.other", OrganizationID: "org-a"},
		"another org":         {Dir: cfg.Dir, ProfileName: cfg.ProfileName, AuthURL: cfg.AuthURL, OrganizationID: "org-b"},
	} {
		if got := signedInUser(&c); got != (relay.User{}) {
			t.Errorf("%s: named %+v", name, got)
		}
	}
}

// An anonymous agent's parts name the signed-in user, email and all, under the rule a heartbeat
// names them by: only those bound for the selected team's validated policy at this profile's
// ingest, as the configuration is when the relay asks, not as it started.
func TestRelayUserFollowsTheHeartbeatsRule(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	authSandbox(t, &fakeAuth{srv: srv})
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_a", RefreshToken: "ter_clr_a",
		ExpiresAt: time.Now().Add(time.Hour), AuthURL: srv.URL, OrganizationID: "org-a", UserID: "user-a", UserEmail: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization("org-a", "A"); p.OTLPURL = srv.URL }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_OTLP_URL", "")
	if got := testApp.relayUser("team", srv.URL); got != (relay.User{}) {
		t.Fatalf("with no team policy fetched, named %+v", got)
	}
	selectPolicy(t, config.Policy{Mode: config.ModeRepo, TeamID: "team", Revision: 1, FetchedAt: time.Now()})
	want := relay.User{ID: "user-a", Email: "a@example.com"}
	if got := testApp.relayUser("team", srv.URL); got != want {
		t.Fatalf("bound for the team, named %+v, want %+v", got, want)
	}
	for name, to := range map[string][2]string{
		"another team":   {"elsewhere", srv.URL},
		"another ingest": {"team", "https://otel.other.example"},
	} {
		if got := testApp.relayUser(to[0], to[1]); got != (relay.User{}) {
			t.Errorf("%s: named %+v", name, got)
		}
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.ServerKeySignIn = true }); err != nil {
		t.Fatal(err)
	}
	if got := testApp.relayUser("team", srv.URL); got != (relay.User{}) {
		t.Errorf("set up with a server key, named %+v", got)
	}
}
