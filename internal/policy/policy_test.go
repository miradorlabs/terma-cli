package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", stub)
	return &config.Config{ProfileName: config.DefaultProfile, OrganizationID: org, AuthURL: authURL, ProjectID: team}
}

func cache(t *testing.T, fetched time.Time) config.Policy {
	t.Helper()
	p := config.Policy{Mode: config.ModeRepo, Signals: []string{"logs"}, TeamID: team, OrganizationID: org, AuthURL: authURL, FetchedAt: fetched}
	if err := routing.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A fetched policy is stamped with the login and team it was fetched for, and stored.
func TestRefreshStoresThePolicyForItsTeam(t *testing.T) {
	cfg := setUp(t, `{"mode":"repo","signals":["metrics"]}`)
	if err := (Source{}).Refresh(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	stored, ok := routing.ValidatedPolicy(cfg, team)
	if !ok || stored.TeamID != team || stored.OrganizationID != org || stored.AuthURL != authURL || stored.Signals[0] != "metrics" {
		t.Fatalf("stored %+v, %v", stored, ok)
	}
}

// A fresh policy is used as it is; a stale one whose refresh fails is kept until it
// expires; a team never validated gets nothing.
func TestCurrentKeepsTheLastValidatedPolicy(t *testing.T) {
	cfg := setUp(t, refused)
	fresh := cache(t, time.Now())
	if got, err := (Source{}).Current(t.Context(), cfg, team); err != nil || !got.FetchedAt.Equal(fresh.FetchedAt) {
		t.Fatalf("fresh: %+v, %v", got, err)
	}

	cfg = setUp(t, refused)
	stale := cache(t, time.Now().Add(-time.Hour))
	if got, err := (Source{}).Current(t.Context(), cfg, team); err != nil || !got.FetchedAt.Equal(stale.FetchedAt) {
		t.Fatalf("stale with a failed refresh: %+v, %v", got, err)
	}

	cfg = setUp(t, refused)
	cache(t, time.Now().Add(-config.MaxPolicyAge-time.Hour))
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
	if _, err := auth.SaveCredential(cfg.ProfileName, &auth.Credential{AccessToken: "a", AuthURL: authURL, OrganizationID: "org_b", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Source{}).Fetch(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "another organization") {
		t.Fatalf("Fetch = %v", err)
	}
}
