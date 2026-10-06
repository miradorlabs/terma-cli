package policy

import (
	"bytes"
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
