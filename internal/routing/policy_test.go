package routing

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Each team's sessions follow that team's stored policy: never another team's, another
// login's, or an expired one.
func TestEffectivePolicyIsolatesTeamsAndExpires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := config.DefaultPolicy()
	a.TeamID, a.OrganizationID, a.AuthURL, a.FetchedAt, a.IncludePrompts = "team-a", "org", "https://dev.example", time.Now(), false
	b := a
	b.TeamID, b.IncludePrompts = "team-b", true
	for _, p := range []config.Policy{a, b} {
		if err := config.WritePolicy(dir, p); err != nil {
			t.Fatal(err)
		}
	}
	if EffectivePolicy(dir, b, "team-a").IncludePrompts || !EffectivePolicy(dir, a, "team-b").IncludePrompts {
		t.Fatal("borrowed another team's capture policy")
	}
	if !EffectivePolicy(dir, b, "unknown").CollectsNothing {
		t.Fatal("an unknown team borrowed the selected team's grant")
	}
	other := b
	other.OrganizationID = "other"
	if !EffectivePolicy(dir, other, "team-b").CollectsNothing {
		t.Fatal("a policy crossed organizations")
	}
	a.FetchedAt = time.Now().Add(-config.MaxPolicyAge - time.Hour)
	if err := config.WritePolicy(dir, a); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePolicy(dir, b, "team-a"); !got.CollectsNothing || got.IncludePrompts {
		t.Fatalf("an expired policy granted %+v", got)
	}
}

// Every fetcher stores through StorePolicy, which refuses another login's organization or
// environment, or an older revision: no caller has to remember to check.
func TestStorePolicyRefusesAnotherLoginsOrAnOlderPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{Dir: dir, StateDir: dir, ProfileName: "default", OrganizationID: "org_a", AuthURL: "https://auth.example"}
	for _, pol := range []config.Policy{
		{Mode: config.ModeRepo, TeamID: "t1", OrganizationID: "org_b", AuthURL: "https://auth.example", FetchedAt: time.Now()},
		{Mode: config.ModeRepo, TeamID: "t1", OrganizationID: "org_a", AuthURL: "https://auth.other", FetchedAt: time.Now()},
	} {
		if err := StorePolicy(cfg, &pol); err == nil {
			t.Errorf("stored %+v under %s/%s", pol, cfg.OrganizationID, cfg.AuthURL)
		}
	}
	if _, ok, err := config.ReadPolicy(dir, "t1"); err != nil || ok {
		t.Fatalf("a refused policy was stored: %v", err)
	}

	pol := config.Policy{Mode: config.ModeRepo, TeamID: "t1", OrganizationID: "org_a", AuthURL: "https://auth.example", Revision: 9, FetchedAt: time.Now()}
	if err := StorePolicy(cfg, &pol); err != nil {
		t.Fatal(err)
	}
	older := pol
	older.Revision = 8
	if err := StorePolicy(cfg, &older); err == nil {
		t.Fatal("accepted an older revision")
	}
}

// Two profiles on one machine select two teams: each team's policy is kept, a profile
// keeps the team it selected while another team's policy is refreshed, and a refresh
// never rewrites config.json.
func TestEachTeamKeepsItsPolicyAndEachProfileItsTeam(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	scoped := func(profile string, team string) *config.Config {
		return &config.Config{Dir: dir, StateDir: dir, ProfileName: profile, OrganizationID: "org_a", AuthURL: "https://auth.example",
			Policy: config.Policy{TeamID: team}}
	}
	policy := func(team string) *config.Policy {
		return &config.Policy{Mode: config.ModeRepo, TeamID: team, OrganizationID: "org_a", AuthURL: "https://auth.example", Revision: 1, FetchedAt: time.Now()}
	}
	// Setup under each profile selects its team.
	if err := StorePolicy(scoped("default", "live"), policy("live")); err != nil {
		t.Fatal(err)
	}
	if err := StorePolicy(scoped("other", "other"), policy("other")); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(config.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	// The relay, under the default profile, refreshes both teams.
	for _, team := range []string{"live", "other"} {
		if err := StorePolicy(scoped("default", "live"), policy(team)); err != nil {
			t.Fatal(err)
		}
	}
	for profile, team := range map[string]string{"default": "live", "other": "other"} {
		got, err := config.Load(dir, dir, config.Overrides{Profile: profile, AuthURL: "https://auth.example"})
		if err != nil || got.Policy.TeamID != team || !got.Policy.Validated() {
			t.Fatalf("profile %s applies %+v, %v; want team %s", profile, got.Policy, err, team)
		}
	}
	if after, err := os.ReadFile(config.Path(dir)); err != nil || !bytes.Equal(after, settings) {
		t.Fatalf("a refresh rewrote config.json:\n%s", after)
	}
}
