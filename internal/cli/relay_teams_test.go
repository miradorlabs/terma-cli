package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const (
	acmeKey     = "ter_srv_acme0123456789abcdefghijklmnopqrstuvw"
	personalKey = "ter_srv_mine0123456789abcdefghijklmnopqrstuvw"
)

// migrateTestMachine marks testApp's machine migrated (config.Migrated) and points it at
// host, for auth and ingest alike.
func migrateTestMachine(t *testing.T, host string) {
	t.Helper()
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_AUTH_URL", host)
	t.Setenv("TERMA_OTLP_URL", host)
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// selectTestTeams writes the committed routing state: acme, then personal, both Active,
// each with an explicit organization, environment and team.
func selectTestTeams(t *testing.T, host string) (acme, personal string) {
	t.Helper()
	now := time.Now()
	pol := func(team, org string, repos ...string) config.Policy {
		p := config.Policy{Mode: config.ModeRepo, Repositories: repos, OrganizationID: org, AuthURL: host, TeamID: team, Revision: 1, FetchedAt: now}
		if repos == nil {
			p.Mode = config.ModeGlobal
		}
		return p
	}
	st, err := routing.Create(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
		if _, err := s.Select(routing.Team{OrganizationID: "org-acme", AuthURL: host, TeamID: "acme", Policy: pol("acme", "org-acme", "github.com/acme/api")}); err != nil {
			return err
		}
		if _, err := s.Select(routing.Team{OrganizationID: "org-me", AuthURL: host, TeamID: "me", Personal: true, Policy: pol("me", "org-me")}); err != nil {
			return err
		}
		s.Migration = &routing.Migration{Phase: routing.Committed}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return st.Teams[0].EnrollmentID, st.Teams[1].EnrollmentID
}

func signInTo(t *testing.T, host, org, user string) {
	t.Helper()
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, &auth.Credential{AccessToken: "ter_cli_" + org, RefreshToken: "ter_clr_" + org,
		ExpiresAt: time.Now().Add(time.Hour), AuthURL: host, OrganizationID: org, UserID: user}); err != nil {
		t.Fatal(err)
	}
}

// A team's key is created with its own organization's sign-in, never the profile's active
// one, named for this machine and the team's enrollment.
func TestATeamKeyIsCreatedWithItsOrganizationsSignIn(t *testing.T) {
	var mu sync.Mutex
	var bearer string
	var req createServerKeyRequestShape
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api-keys/server" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bearer = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"key": acmeKey, "server_key": map[string]string{"id": "k1"}})
	}))
	defer srv.Close()
	migrateTestMachine(t, srv.URL)
	signInTo(t, srv.URL, "org-acme", "user-acme")
	signInTo(t, srv.URL, "org-me", "user-me") // the active sign-in
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	team := routing.Team{EnrollmentID: "0123456789abcdef0123456789abcdef", OrganizationID: "org-acme", AuthURL: srv.URL, TeamID: "acme"}
	key, err := testApp.createTeamKey(t.Context(), cfg, team)
	if err != nil || key != acmeKey {
		t.Fatalf("createTeamKey = %q, %v", key, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bearer != "Bearer ter_cli_org-acme" || req.ProjectID != "acme" || !strings.HasSuffix(req.Name, "/"+team.EnrollmentID) || !strings.HasPrefix(req.Name, "terma@") {
		t.Fatalf("asked with %q for %+v", bearer, req)
	}
}

// On a migrated machine the heartbeat goes to one selected, Active team with its own key:
// personal first, else the first team, naming that team's organization's user.
func TestAMigratedHeartbeatGoesToOneTeamWithItsKey(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var beat logspb.LogsData
		_ = proto.Unmarshal(body, &beat)
		user := ""
		for _, kv := range beat.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0].GetAttributes() {
			if kv.GetKey() == semconv.TermaUserIDKey {
				user = kv.GetValue().GetStringValue()
			}
		}
		mu.Lock()
		sent = append(sent, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+"="+user)
		mu.Unlock()
	}))
	defer srv.Close()
	migrateTestMachine(t, srv.URL)
	signInTo(t, srv.URL, "org-acme", "user-acme")
	signInTo(t, srv.URL, "org-me", "user-me")
	acme, personal := selectTestTeams(t, srv.URL)
	// A key kept from before the migration is never used.
	if err := keystore.Set(testApp.dir, "acme", mintedKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	beat := func() error {
		return testApp.relayHeartbeatSend(t.Context(), &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}})
	}
	if err := beat(); err == nil {
		t.Fatal("a beat went without any team's key")
	}
	for id, key := range map[string]string{acme: acmeKey, personal: personalKey} {
		if err := keystore.SetTeam(testApp.dir, id, map[string]string{acme: "acme", personal: "me"}[id], key, keystore.Hosts{OTLP: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if err := beat(); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
		s.Teams[1].Refusals = 1 // personal is Suspended
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := beat(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != personalKey+"=user-me" || sent[1] != acmeKey+"=user-acme" {
		t.Fatalf("beats sent %v", sent)
	}
}

// The relay's dependencies carry what a migrated machine needs, and the ingest pinned.
func TestTheRelayDepsServeAMigratedMachine(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_OTLP_URL", "")
	d := testApp.relayDeps()
	if d.RefreshTeam == nil || d.CreateTeamKey == nil || d.TeamUser == nil || d.OTLPPinned {
		t.Fatalf("deps %+v", d)
	}
	t.Setenv("TERMA_OTLP_URL", "https://otel.example")
	if !testApp.relayDeps().OTLPPinned {
		t.Fatal("a pinned ingest is not pinned")
	}
}
