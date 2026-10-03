package daemon

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// A relay started before any policy was fetched captures nothing until one is.
func TestARelayCapturesNothingBeforeTheFirstFetch(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	for _, pol := range []config.Policy{
		{},
		{Mode: config.ModeRepo, IncludePrompts: true, TeamID: "t1"},
	} {
		cfg := &config.Config{OrganizationID: "org_a", AuthURL: "https://auth.example", Policy: pol}
		Prepare(cfg)
		if cfg.Policy.IncludePrompts || cfg.Policy.IncludeToolContent || !cfg.Policy.CollectsNothing || cfg.Policy.Global() {
			t.Errorf("Prepare(%+v) = %+v", pol, cfg.Policy)
		}
	}
	fetched := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, TeamID: "t1", FetchedAt: time.Now()}
	cfg := &config.Config{Policy: fetched}
	if Prepare(cfg); cfg.Policy.CollectsNothing || !cfg.Policy.IncludePrompts {
		t.Fatalf("a validated policy was replaced: %+v", cfg.Policy)
	}
}

// The environment only ever shortens the relay's timings.
func TestSettingsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("TERMA_RELAY_HOLD", "3s")
	t.Setenv("TERMA_RELAY_HEARTBEAT", "-1s")
	t.Setenv("TERMA_RELAY_DEBUG", "1")
	if s := SettingsFromEnv(); s.Hold != 3*time.Second || s.Heartbeat != 0 || !s.Debug {
		t.Fatalf("SettingsFromEnv = %+v", s)
	}
	t.Setenv("TERMA_RELAY_HOLD", "nonsense")
	t.Setenv("TERMA_RELAY_DEBUG", "")
	if s := SettingsFromEnv(); s.Hold != relay.DefaultHold || s.Debug {
		t.Fatalf("SettingsFromEnv = %+v", s)
	}
}

// Assembling the relay reaches nothing: no key is minted and no policy fetched until the
// relay runs and a claim asks.
func TestTheEngineStartsNothing(t *testing.T) {
	d := Deps{
		CreateKey: func(context.Context, *config.Config, string) (string, error) {
			t.Fatal("minted while assembling")
			return "", nil
		},
		RefreshPolicy: func(context.Context, *config.Config) error { t.Fatal("fetched while assembling"); return nil },
		HookPolicy:    func() config.Policy { return config.Policy{} },
	}
	var log bytes.Buffer
	opts := d.Engine(t.Context(), t.TempDir(), &config.Config{}, Settings{Hold: time.Second, Heartbeat: time.Minute}, &log)
	if opts.Hold != time.Second || opts.HeartbeatEvery != time.Minute || opts.Resolve == nil || opts.CatchAll == nil || opts.Logf != nil {
		t.Fatalf("Engine = %+v", opts)
	}
	if opts := d.Engine(t.Context(), t.TempDir(), &config.Config{}, Settings{Debug: true}, &log); opts.Logf == nil {
		t.Fatal("debug settings logged nothing")
	}
}

// The refresher keeps the profile's selected team fresh even with no key for it, and
// refreshes each team under that team's own project.
func TestTheRefresherKeepsTheSelectedTeamFresh(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	var refreshed []string
	d := Deps{
		LoadConfig: func() (*config.Config, error) {
			return &config.Config{Policy: config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "t1"}}, nil
		},
		RefreshPolicy: func(_ context.Context, cfg *config.Config) error {
			refreshed = append(refreshed, cfg.ProjectID)
			return nil
		},
	}
	r := d.Refresher()
	if teams := r.Teams(); !slices.Equal(teams, []string{"t1"}) {
		t.Fatalf("Teams = %v", teams)
	}
	if !r.Fetched("t1").IsZero() {
		t.Fatal("a team never fetched has a fetch time")
	}
	if err := r.Refresh(t.Context(), "t1"); err != nil || !slices.Equal(refreshed, []string{"t1"}) {
		t.Fatalf("Refresh = %v, refreshed %v", err, refreshed)
	}
}

// A team with its key on file and no policy fetched is held, never granted; the relay's refresher finds it at once by its key, and once that fetch is
// stored the team's records follow its policy.
func TestAFreshBindingIsHeldUntilTheRelayFetchesItsPolicy(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	const org, auth = "org_a", "https://auth.example"
	t.Setenv("TERMA_AUTH_URL", auth)
	cfg := &config.Config{ProfileName: "default", OrganizationID: org, AuthURL: auth}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { p.OrganizationID, p.Harnesses = org, []string{"claude"} }); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set("p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "claude" }, Endpoint: func(string) string { return "https://otel.example" },
		RelayTargets: func(s []string) []string { return s }})
	c := claim.Claim{ProjectID: "p1", Tool: "claude"}
	if pol, err := resolve(c); err == nil {
		t.Fatalf("a team with no policy fetched was granted %+v", pol)
	}

	var fetched []string
	d := Deps{
		LoadConfig: func() (*config.Config, error) { return config.Load(config.Overrides{}) },
		RefreshPolicy: func(_ context.Context, scoped *config.Config) error {
			fetched = append(fetched, scoped.ProjectID)
			pol := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, OrganizationID: org, AuthURL: auth,
				TeamID: scoped.ProjectID, Revision: 1, FetchedAt: time.Now()}
			return routing.StorePolicy(scoped, &pol)
		},
	}
	r := d.Refresher()
	if !slices.Contains(r.Teams(), "p1") || !r.Fetched("p1").IsZero() {
		t.Fatalf("the fresh team was not due at once: teams %v", r.Teams())
	}
	if err := r.Refresh(t.Context(), "p1"); err != nil || !slices.Equal(fetched, []string{"p1"}) {
		t.Fatalf("Refresh = %v, fetched %v", err, fetched)
	}
	pol, err := resolve(c)
	if err != nil || !pol.IncludePrompts || pol.IncludeToolContent || pol.Signals != nil {
		t.Fatalf("after the fetch: %+v, %v; want the team's prompts-only policy", pol, err)
	}
}
