package daemon

import (
	"cmp"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A policy from another environment, or none validated yet, grants nothing. One from
// another organization the profile signed into grants, as does the selected team's once
// the profile has signed into another organization since: one machine collects for every
// organization it is signed into.
func TestResolverGrantsOnlyAValidatedPolicyOfThisEnvironment(t *testing.T) {
	const org, auth = "org_a", "https://auth.example"
	fetched := time.Now()
	valid := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true,
		OrganizationID: org, AuthURL: auth, TeamID: "p1", FetchedAt: fetched}
	for _, tc := range []struct {
		name   string
		policy func() *config.Policy
		org    string
		ok     bool
	}{
		{name: "validated", policy: func() *config.Policy { return &valid }, ok: true},
		{name: "never fetched", policy: func() *config.Policy { v := valid; v.FetchedAt = time.Time{}; return &v }},
		{name: "no policy", policy: func() *config.Policy { return nil }},
		{name: "another organization", policy: func() *config.Policy { v := valid; v.OrganizationID = "org_b"; return &v }, ok: true},
		{name: "another environment", policy: func() *config.Policy { v := valid; v.AuthURL = "https://auth.other"; return &v }},
		{name: "signed in elsewhere since", policy: func() *config.Policy { return &valid }, org: "org_b", ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("TERMA_POLICY_STUB", "")
			cfg := &config.Config{Dir: configDir, StateDir: configDir, ProfileName: "default", OrganizationID: org, AuthURL: auth}
			if err := config.UpdateProfile(configDir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID, p.Team = cmp.Or(tc.org, org), "p1" }); err != nil {
				t.Fatal(err)
			}
			if pol := tc.policy(); pol != nil {
				if err := config.WritePolicy(configDir, *pol); err != nil {
					t.Fatal(err)
				}
			}
			if err := keystore.Set(configDir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
				t.Fatal(err)
			}
			resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s }})
			pol, err := resolve(claim.Claim{ProjectID: "p1"})
			if tc.ok {
				if err != nil || !pol.IncludePrompts || pol.Key != mintedKey {
					t.Fatalf("a validated policy was not applied: %+v, %v", pol, err)
				}
				return
			}
			if err == nil && (pol.IncludePrompts || pol.IncludeToolContent || len(pol.Signals) > 0) {
				t.Fatalf("granted %+v", pol)
			}
		})
	}
}

// A profile removed while the relay runs grants nothing: the policy it started with is
// not evidence of the one in force.
func TestResolverGrantsNothingOnceTheProfileIsGone(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TERMA_POLICY_STUB", "")
	cfg := &config.Config{Dir: configDir, ProfileName: "default", OrganizationID: "org_a",
		Policy: config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org_a", TeamID: "p1", FetchedAt: time.Now()}}
	if err := keystore.Set(configDir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s }})
	if pol, err := resolve(claim.Claim{ProjectID: "p1"}); err == nil {
		t.Fatalf("granted %+v with no profile on file", pol)
	}
}

// A key the keychain will not give up now holds its parts back without minting another.
func TestResolverMintsNothingWhileTheKeychainIsLocked(t *testing.T) {
	configDir := t.TempDir()
	cfg := &config.Config{Dir: configDir, ProfileName: "default"}
	if err := keystore.Set(configDir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, configDir)
	minted := false
	resolve := Resolver(cfg, ResolverDeps{Mint: func(string) { minted = true }, AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s }})
	_, err := resolve(claim.Claim{ProjectID: "p1"})
	if !secret.IsUnavailable(err) || errors.Is(err, relay.ErrNoKey) || minted {
		t.Fatalf("resolve = %v, minted %v; want the keychain unavailable and no mint", err, minted)
	}
}

// A locked keychain is not asked again at every resolve: on Linux each ask can raise an
// unlock prompt. Unlocked meanwhile, it is still answered from the last refusal.
func TestResolverWaitsBeforeAskingALockedKeychainAgain(t *testing.T) {
	configDir := t.TempDir()
	cfg := &config.Config{Dir: configDir, ProfileName: "default"}
	if err := keystore.Set(configDir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	unlock := secret.FailForTest(t, configDir)
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s }})
	if _, err := resolve(claim.Claim{ProjectID: "p1"}); !secret.IsUnavailable(err) {
		t.Fatalf("first resolve = %v", err)
	}
	unlock()
	if _, err := resolve(claim.Claim{ProjectID: "p1"}); !secret.IsUnavailable(err) {
		t.Fatalf("a resolve inside %s asked the keychain again: %v", keychainRetry, err)
	}
}

// A claim for a team this machine no longer collects for — its validated policy file and
// key still here, the team selected in no organization since — is granted nothing: hooks
// chose that team before a switch, and the session is one nothing places now.
func TestResolverGrantsOnlyATeamTheMachineCollectsFor(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TERMA_POLICY_STUB", "")
	const org, auth = "org_a", "https://auth.example"
	cfg := &config.Config{Dir: configDir, StateDir: configDir, ProfileName: "default", ProfileEnvironment: config.EnvProd, OrganizationID: org, AuthURL: auth}
	if err := config.UpdateProfile(configDir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID = org; p.SelectTeam("p1") }); err != nil {
		t.Fatal(err)
	}
	for _, team := range []string{"p1", "p2"} {
		pol := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, OrganizationID: org, AuthURL: auth, TeamID: team, FetchedAt: time.Now()}
		if err := config.WritePolicy(configDir, pol); err != nil {
			t.Fatal(err)
		}
		if err := keystore.Set(configDir, team, mintedKey, keystore.HostsOf(cfg)); err != nil {
			t.Fatal(err)
		}
	}
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s },
		LoadProfile: func(name string) (*config.Config, error) {
			return config.Load(configDir, configDir, config.Overrides{Profile: name, AuthURL: auth})
		}})
	if pol, err := resolve(claim.Claim{ProjectID: "p1"}); err != nil || !pol.IncludePrompts {
		t.Fatalf("the selected team was not granted: %+v, %v", pol, err)
	}
	// Held as a team with no validated policy is, never granted: its exports wait out the
	// relay's hold, so a profile selecting it meanwhile loses nothing.
	if pol, err := resolve(claim.Claim{ProjectID: "p2"}); err == nil || !strings.Contains(err.Error(), "no validated collection policy") {
		t.Fatalf("a team selected nowhere: %+v, %v", pol, err)
	}
	// Another profile selects p2: its hooks claim for it, and the one relay forwards for it.
	if err := config.UpdateProfile(configDir, "other", func(p *config.Profile) { p.OrganizationID = org; p.SelectTeam("p2") }); err != nil {
		t.Fatal(err)
	}
	if pol, err := resolve(claim.Claim{ProjectID: "p2"}); err != nil || !pol.IncludePrompts {
		t.Fatalf("another profile's team was not granted: %+v, %v", pol, err)
	}
}

// A claim for a team only another profile selected is judged under that profile: its own
// global rule and its own agents, so a Codex session of a profile that chose Codex is
// granted though the relay's profile chose Claude alone, and that profile's global team
// is the honoured one for its sessions. A profile of another environment is not judged
// here: its claims are held.
func TestResolverJudgesAClaimUnderTheProfileThatSelectedItsTeam(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TERMA_POLICY_STUB", "")
	const org, auth = "org_a", "https://auth.example"
	cfg := &config.Config{Dir: configDir, StateDir: configDir, ProfileName: "default", ProfileEnvironment: config.EnvProd, OrganizationID: org, AuthURL: auth, Harnesses: []string{"claude"}}
	if err := config.UpdateProfile(configDir, "default", func(p *config.Profile) { p.OrganizationID, p.Harnesses = org, []string{"claude"}; p.SelectTeam("p1") }); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(configDir, "other", func(p *config.Profile) { p.OrganizationID, p.Harnesses = org, []string{"codex"}; p.SelectTeam("p2") }); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(configDir, "dev", func(p *config.Profile) {
		p.OrganizationID, p.Harnesses, p.Environment = org, []string{"codex"}, "dev"
		p.SelectTeam("p3")
	}); err != nil {
		t.Fatal(err)
	}
	for team, mode := range map[string]string{"p1": config.ModeRepo, "p2": config.ModeGlobal, "p3": config.ModeGlobal} {
		pol := config.Policy{Mode: mode, IncludePrompts: true, IncludeToolContent: true, OrganizationID: org, AuthURL: auth, TeamID: team, FetchedAt: time.Now()}
		if mode == config.ModeGlobal {
			pol.DefaultProjectID = team
		}
		if err := config.WritePolicy(configDir, pol); err != nil {
			t.Fatal(err)
		}
		if err := keystore.Set(configDir, team, mintedKey, keystore.HostsOf(cfg)); err != nil {
			t.Fatal(err)
		}
	}
	load := func(name string) (*config.Config, error) {
		return config.Load(configDir, configDir, config.Overrides{Profile: name, AuthURL: auth})
	}
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(tool string) string { return tool }, Endpoint: func(string) string { return "https://otel.example" },
		RelayTargets: func(s []string) []string { return s }, LoadProfile: load})
	pol, err := resolve(claim.Claim{ProjectID: "p2", Tool: "codex"})
	if err != nil || !pol.IncludePrompts || len(pol.Signals) != 0 && pol.Signals != nil || pol.RequireClaim {
		t.Fatalf("another profile's Codex session under its global team: %+v, %v", pol, err)
	}
	// The relay's own profile still judges its own team with its own agents: Codex is not among them.
	if pol, err := resolve(claim.Claim{ProjectID: "p1", Tool: "codex"}); err != nil || pol.IncludePrompts || pol.Signals == nil || len(pol.Signals) != 0 {
		t.Fatalf("the relay profile's unchosen agent was granted: %+v, %v", pol, err)
	}
	if pol, err := resolve(claim.Claim{ProjectID: "p3", Tool: "codex"}); err == nil || !strings.Contains(err.Error(), "no validated collection policy") {
		t.Fatalf("another environment's profile was judged here: %+v, %v", pol, err)
	}
	// Without the hook, every claim is judged under the relay's profile, as before.
	alone := Resolver(cfg, ResolverDeps{AgentName: func(tool string) string { return tool }, Endpoint: func(string) string { return "https://otel.example" }, RelayTargets: func(s []string) []string { return s }})
	if pol, err := alone(claim.Claim{ProjectID: "p2", Tool: "codex"}); err == nil || !strings.Contains(err.Error(), "no validated collection policy") {
		t.Fatalf("without LoadProfile another profile's team was judged: %+v, %v", pol, err)
	}
}
