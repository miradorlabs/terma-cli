package policy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
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

// A team of another organization in the collection is fetched with that organization's
// stored credential, never the active one's.
func TestFetchUsesTheTeamsOrganizationsCredential(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/policy" || r.URL.Query().Get("project_id") != "tb" || r.Header.Get("Authorization") != "Bearer token_b" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"policy":{"version":"1.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":true},` +
			`"per_repository":{"repositories":["github.com/beta/site"]}}},"revision":3,"updated_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	// org_a signed in last, so it is the active credential.
	for _, c := range []*auth.Credential{
		{AccessToken: "token_b", AuthURL: srv.URL, OrganizationID: "org_b", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "token_a", AuthURL: srv.URL, OrganizationID: "org_a", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if _, err := auth.SaveCredential(dir, config.DefaultProfile, c); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Dir: dir, StateDir: t.TempDir(), ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL,
		OrganizationID: "org_b", OrganizationName: "Beta", ProjectID: "tb", ProjectName: "Web"}
	pol, err := (Source{}).Fetch(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Fetch: %v (tokens sent: %v)", err, tokens)
	}
	if pol.OrganizationID != "org_b" || pol.OrganizationName != "Beta" || pol.TeamID != "tb" || pol.TeamName != "Web" ||
		!pol.IncludePrompts || pol.IncludeToolContent || len(pol.Repositories) != 1 {
		t.Fatalf("fetched %+v", pol)
	}
	if len(tokens) != 1 || tokens[0] != "Bearer token_b" {
		t.Fatalf("tokens sent: %v", tokens)
	}
}
