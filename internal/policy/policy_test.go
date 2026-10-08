package policy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	org     = "org_a"
	authURL = "https://auth.example"
	team    = "t1"
	// refused stands in for a fetch that fails: the stub's mode is invalid.
	refused = `{"mode":"bogus"}`
)

func setUp(t *testing.T, stub string) *config.Config {
	t.Helper()
	t.Setenv("TERMA_POLICY_STUB", stub)
	return &config.Config{Dir: t.TempDir(), StateDir: t.TempDir(), ProfileName: config.DefaultProfile, OrganizationID: org, AuthURL: authURL, ProjectID: team}
}

func cache(cfg *config.Config, fetched time.Time) config.Policy {
	cfg.Policy = config.Policy{Mode: config.ModeRepo, TeamID: team, OrganizationID: org, AuthURL: authURL, FetchedAt: fetched}
	return cfg.Policy
}

// A fetched policy is stamped with the login and team it was fetched for, and stored in
// the state directory; refreshing it leaves the developer's config.json alone.
func TestRefreshStoresThePolicyForItsTeam(t *testing.T) {
	cfg := setUp(t, `{"mode":"repo","include_prompts":true}`)
	if err := config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID, p.Team = org, team }); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(config.Path(cfg.Dir))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := (Source{}).Refresh(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	stored, ok, err := config.ReadPolicy(cfg.StateDir, team)
	if err != nil || !ok || stored.TeamID != team || stored.OrganizationID != org || stored.AuthURL != authURL || !stored.IncludePrompts {
		t.Fatalf("stored %+v", stored)
	}
	if after, err := os.ReadFile(config.Path(cfg.Dir)); err != nil || !bytes.Equal(after, settings) {
		t.Fatalf("a refresh rewrote config.json:\n%s", after)
	}
}

// A fresh policy is used as it is; a stale one whose refresh fails is kept until it
// expires; a team never validated gets nothing.
func TestCurrentKeepsTheLastValidatedPolicy(t *testing.T) {
	cfg := setUp(t, refused)
	fresh := cache(cfg, time.Now())
	if got, err := (Source{}).Current(t.Context(), cfg, team); err != nil || !got.FetchedAt.Equal(fresh.FetchedAt) {
		t.Fatalf("fresh: %+v, %v", got, err)
	}

	cfg = setUp(t, refused)
	stale := cache(cfg, time.Now().Add(-time.Hour))
	if got, err := (Source{}).Current(t.Context(), cfg, team); err != nil || !got.FetchedAt.Equal(stale.FetchedAt) {
		t.Fatalf("stale with a failed refresh: %+v, %v", got, err)
	}

	cfg = setUp(t, refused)
	cache(cfg, time.Now().Add(-config.MaxPolicyAge-time.Hour))
	if got, err := (Source{}).Current(t.Context(), cfg, team); err == nil {
		t.Fatalf("expired with a failed refresh: %+v", got)
	}

	cfg = setUp(t, refused)
	if got, err := (Source{}).Current(t.Context(), cfg, team); err == nil {
		t.Fatalf("never validated: %+v", got)
	}
}

// A profile set up with a server key refreshes its team's policy with that team's key from
// the keystore, TERMA_API_KEY unset and a login on file; a login profile fetches with the
// login, even with TERMA_API_KEY set.
func TestFetchUsesTheProfilesCredential(t *testing.T) {
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path != "/v1/policy" || r.URL.Query().Get("project_id") != team {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"global":{}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_login", AuthURL: srv.URL, OrganizationID: org, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(cfg.Dir, team, "ter_srv_team", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}

	cfg.ServerKeySignIn, cfg.Team = true, team
	if err := (Source{}).Refresh(t.Context(), cfg); err != nil || bearer != "ter_srv_team" {
		t.Fatalf("server-key profile: %v, sent %q", err, bearer)
	}
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, team); err != nil || !ok || stored.TeamID != team || !stored.Global() {
		t.Fatalf("stored %+v, %v, %v", stored, ok, err)
	}

	cfg.ServerKeySignIn, cfg.APIKey = false, "ter_srv_env"
	if _, err := (Source{}).Fetch(t.Context(), cfg); err != nil || bearer != "ter_cli_login" {
		t.Fatalf("login profile under TERMA_API_KEY: %v, sent %q", err, bearer)
	}
}

// A profile set up with a server key whose key is gone says how to store it again, and
// never falls back to the login.
func TestFetchWithoutTheServerKeyNamesSetup(t *testing.T) {
	cfg := setUp(t, "")
	cfg.ServerKeySignIn, cfg.Team = true, team
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, &auth.Credential{AccessToken: "a", AuthURL: authURL, OrganizationID: org, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Source{}).Fetch(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "terma setup") {
		t.Fatalf("Fetch = %v", err)
	}
}

// A profile set up with team B's server key fetches no other team's policy, not even with
// a key kept here for team A of another organization, which it would stamp with B's.
func TestFetchWithAServerKeyRefusesAnotherTeam(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"global":{}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	cfg := setUp(t, "")
	cfg.AuthURL, cfg.OrganizationID, cfg.Team, cfg.ServerKeySignIn = srv.URL, "org_b", "team-b", true
	for id, key := range map[string]string{team: "ter_srv_a", "team-b": "ter_srv_b"} {
		if err := keystore.Set(cfg.Dir, id, key, keystore.Hosts{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Source{}).Refresh(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "team-b") || asked != 0 {
		t.Fatalf("Refresh of team A = %v, %d policies asked for", err, asked)
	}
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, team); err != nil || ok {
		t.Fatalf("stored for team A: %+v, %v, %v", stored, ok, err)
	}
}

// A login from another organization fetches nothing for this one.
func TestFetchRefusesAnotherOrganizationsLogin(t *testing.T) {
	cfg := setUp(t, "")
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, &auth.Credential{AccessToken: "a", AuthURL: authURL, OrganizationID: "org_b", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Source{}).Fetch(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "another organization") {
		t.Fatalf("Fetch = %v", err)
	}
}
