package daemon

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// A policy from another organization or environment, or none validated yet, grants nothing.
func TestResolverGrantsOnlyAValidatedPolicyOfThisLogin(t *testing.T) {
	const org, auth = "org_a", "https://auth.example"
	fetched := time.Now()
	valid := config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true,
		OrganizationID: org, AuthURL: auth, TeamID: "p1", FetchedAt: fetched}
	for _, tc := range []struct {
		name    string
		profile func(*config.Profile)
		team    *config.Policy
		ok      bool
	}{
		{name: "validated", profile: func(p *config.Profile) { p.Policy = &valid }, ok: true},
		{name: "never fetched", profile: func(p *config.Profile) { v := valid; v.FetchedAt = time.Time{}; p.Policy = &v }},
		{name: "no policy", profile: func(p *config.Profile) { p.Policy = nil }},
		{name: "another organization", profile: func(p *config.Profile) { v := valid; v.OrganizationID = "org_b"; p.Policy = &v }},
		{name: "another environment", profile: func(p *config.Profile) { v := valid; v.AuthURL = "https://auth.other"; p.Policy = &v }},
		{name: "signed in elsewhere since", profile: func(p *config.Profile) { p.OrganizationID = "org_b"; p.Policy = &valid }},
		{name: "team cache from another organization", profile: func(p *config.Profile) { p.Policy = &valid },
			team: &config.Policy{Mode: config.ModeRepo, IncludePrompts: true, OrganizationID: "org_b", AuthURL: auth, TeamID: "p1", FetchedAt: fetched}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			t.Setenv("TERMA_POLICY_STUB", "")
			cfg := &config.Config{ProfileName: "default", OrganizationID: org, AuthURL: auth}
			if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
				p.OrganizationID = org
				tc.profile(p)
			}); err != nil {
				t.Fatal(err)
			}
			if tc.team != nil {
				if err := routing.SavePolicy(*tc.team); err != nil {
					t.Fatal(err)
				}
			}
			if err := keystore.Set("p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
				t.Fatal(err)
			}
			resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }})
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	cfg := &config.Config{ProfileName: "default", OrganizationID: "org_a",
		Policy: config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org_a", TeamID: "p1", FetchedAt: time.Now()}}
	if err := keystore.Set("p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(string) string { return "" }, Endpoint: func(string) string { return "https://otel.example" }})
	if pol, err := resolve(claim.Claim{ProjectID: "p1"}); err == nil {
		t.Fatalf("granted %+v with no profile on file", pol)
	}
}
