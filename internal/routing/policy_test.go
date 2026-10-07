package routing

import (
	"bytes"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Each team's sessions follow that team's stored policy: never another team's, another
// environment's, or an expired one. Another organization's is that team's own: one
// machine collects for several organizations.
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
	if got := EffectivePolicy(dir, other, "team-b"); got.CollectsNothing || got.OrganizationID != "org" {
		t.Fatalf("another organization's team lost its own policy: %+v", got)
	}
	elsewhere := b
	elsewhere.AuthURL = "https://prod.example"
	if !EffectivePolicy(dir, elsewhere, "team-b").CollectsNothing {
		t.Fatal("a policy crossed environments")
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

// One machine collects for every organization it is signed into: the collection is the
// selected team's policy first, then every other team's of the environment, with one
// global policy honoured — the selected team's, else the only one — and the rest conflicts.
func TestCollectionSpansOrganizationsAndHonoursOneGlobalPolicy(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	dir := t.TempDir()
	now := time.Now()
	repo := func(team, org string, repos ...string) config.Policy {
		return config.Policy{Mode: config.ModeRepo, TeamID: team, OrganizationID: org, AuthURL: "https://auth.example", Repositories: repos, FetchedAt: now}
	}
	global := func(team, org string) config.Policy {
		return config.Policy{Mode: config.ModeGlobal, TeamID: team, DefaultProjectID: team, OrganizationID: org, AuthURL: "https://auth.example", FetchedAt: now}
	}
	elsewhere := repo("tz", "org_z", "github.com/zeta/app")
	elsewhere.AuthURL = "https://prod.example"
	for _, p := range []config.Policy{repo("ta", "org_a", "github.com/acme/app"), repo("tb", "org_b", "github.com/beta/site"), global("tg", "org_b"), elsewhere} {
		if err := config.WritePolicy(dir, p); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Dir: dir, StateDir: dir, OrganizationID: "org_a", AuthURL: "https://auth.example"}
	cfg.Policy = repo("ta", "org_a", "github.com/acme/app")
	teams := func(ps config.Policies) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.TeamID)
		}
		return out
	}
	if got := teams(Collection(cfg)); !slices.Equal(got, []string{"ta", "tb", "tg"}) {
		t.Fatalf("Collection = %v; want the selected team, the other listing, then the one global policy", got)
	}
	if got := Conflicts(cfg); len(got) != 0 {
		t.Fatalf("a lone global policy is a conflict: %v", teams(got))
	}
	if p, ok := Collection(cfg).Admitting(config.Repository{Origin: "github.com/beta/site"}); !ok || p.TeamID != "tb" || p.OrganizationID != "org_b" {
		t.Fatalf("another organization's repository went to %+v, %v", p, ok)
	}

	// A second unselected global policy: neither is honoured, both are conflicts.
	if err := config.WritePolicy(dir, global("th", "org_a")); err != nil {
		t.Fatal(err)
	}
	if got := teams(Collection(cfg)); !slices.Equal(got, []string{"ta", "tb"}) {
		t.Fatalf("Collection with two global policies = %v", got)
	}
	if got := teams(Conflicts(cfg)); !slices.Equal(got, []string{"tg", "th"}) {
		t.Fatalf("Conflicts = %v", got)
	}

	// The selected team's global policy is the one honoured; every other global one conflicts.
	cfg.Policy = global("ta", "org_a")
	if err := config.WritePolicy(dir, cfg.Policy); err != nil {
		t.Fatal(err)
	}
	if got := teams(Collection(cfg)); !slices.Equal(got, []string{"tb", "ta"}) {
		t.Fatalf("Collection under a selected global policy = %v", got)
	}
	if got := teams(Conflicts(cfg)); !slices.Equal(got, []string{"tg", "th"}) {
		t.Fatalf("Conflicts under a selected global policy = %v", got)
	}
	// Nothing selected: the stored listings still collect, and no global policy is honoured.
	cfg.Policy = config.NoPolicy("org_a", "https://auth.example")
	if got := teams(Collection(cfg)); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("Collection with no selected team = %v", got)
	}
	if got := teams(Conflicts(cfg)); !slices.Equal(got, []string{"ta", "tg", "th"}) {
		t.Fatalf("Conflicts with no selected team = %v", got)
	}
}

// A fetch, mint or refresh for a team runs under the organization whose team it is, with
// the names setup stored, so another organization's credential and labels are used.
func TestScopeToTeamTakesTheTeamsOrganization(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	dir := t.TempDir()
	stored := config.Policy{Mode: config.ModeRepo, TeamID: "tb", OrganizationID: "org_b", OrganizationName: "Beta", TeamName: "Web",
		AuthURL: "https://auth.example", FetchedAt: time.Now()}
	if err := config.WritePolicy(dir, stored); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: dir, StateDir: dir, OrganizationID: "org_a", OrganizationName: "Acme", AuthURL: "https://auth.example", ProjectID: "ta"}
	got := ScopeToTeam(cfg, "tb")
	if got.ProjectID != "tb" || got.OrganizationID != "org_b" || got.OrganizationName != "Beta" || got.ProjectName != "Web" {
		t.Fatalf("ScopeToTeam = %+v", got)
	}
	if cfg.ProjectID != "ta" || cfg.OrganizationID != "org_a" {
		t.Fatal("ScopeToTeam changed the config it was given")
	}
	if got := ScopeToTeam(cfg, "unknown"); got.ProjectID != "unknown" || got.OrganizationID != "org_a" {
		t.Fatalf("an unknown team left the profile's organization: %+v", got)
	}
}

// A refresh of another organization's team stores its policy and keeps the names setup
// recorded, without touching the profile's own selection.
func TestStorePolicyKeepsAnotherOrganizationsTeamApart(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	dir := t.TempDir()
	if err := config.UpdateProfile(dir, "default", func(p *config.Profile) { p.SelectOrganization("org_a", "Acme"); p.Team = "ta" }); err != nil {
		t.Fatal(err)
	}
	first := &config.Policy{Mode: config.ModeRepo, TeamID: "tb", OrganizationID: "org_b", OrganizationName: "Beta", TeamName: "Web", AuthURL: "https://auth.example", Revision: 1, FetchedAt: time.Now()}
	scoped := &config.Config{Dir: dir, StateDir: dir, ProfileName: "default", OrganizationID: "org_b", AuthURL: "https://auth.example", ProjectID: "tb",
		Policy: config.Policy{TeamID: "ta", OrganizationID: "org_a"}}
	if err := StorePolicy(scoped, first); err != nil {
		t.Fatal(err)
	}
	refreshed := *first
	refreshed.OrganizationName, refreshed.TeamName, refreshed.Revision = "", "", 2
	if err := StorePolicy(scoped, &refreshed); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := config.ReadPolicy(dir, "tb")
	if err != nil || !ok || stored.Revision != 2 || stored.OrganizationName != "Beta" || stored.TeamName != "Web" {
		t.Fatalf("stored %+v, %v, %v", stored, ok, err)
	}
	file, err := config.LoadFile(dir)
	if err != nil || file.Profiles["default"].Team != "ta" || file.Profiles["default"].OrganizationID != "org_a" {
		t.Fatalf("the profile's selection moved: %+v, %v", file.Profiles["default"], err)
	}
}
