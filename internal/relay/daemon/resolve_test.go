package daemon

import (
	"cmp"
	"errors"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A policy from another organization or environment, or none validated yet, grants nothing.
func TestResolverGrantsOnlyAValidatedPolicyOfThisLogin(t *testing.T) {
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
		{name: "another organization", policy: func() *config.Policy { v := valid; v.OrganizationID = "org_b"; return &v }},
		{name: "another environment", policy: func() *config.Policy { v := valid; v.AuthURL = "https://auth.other"; return &v }},
		{name: "signed in elsewhere since", policy: func() *config.Policy { return &valid }, org: "org_b"},
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

// Every claim's policy names the user its parts go to, whichever agent claimed it, asked for the
// ingest the policy sends them to.
func TestResolverNamesTheUserForEveryClaim(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TERMA_POLICY_STUB", "")
	const org, auth = "org_a", "https://auth.example"
	cfg := &config.Config{Dir: configDir, StateDir: configDir, ProfileName: "default", OrganizationID: org, AuthURL: auth}
	if err := config.UpdateProfile(configDir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID, p.Team = org, "p1" }); err != nil {
		t.Fatal(err)
	}
	if err := config.WritePolicy(configDir, config.Policy{Mode: config.ModeRepo, OrganizationID: org, AuthURL: auth, TeamID: "p1", FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(configDir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	var asked []string
	resolve := Resolver(cfg, ResolverDeps{AgentName: func(tool string) string { return tool }, Endpoint: func(string) string { return "https://otel.example" },
		RelayTargets: func(s []string) []string { return s },
		User: func(projectID, endpoint string) string {
			asked = append(asked, projectID+"@"+endpoint)
			return "user-a"
		}})
	for _, tool := range []string{"copilot", "claude", ""} {
		pol, err := resolve(claim.Claim{ProjectID: "p1", Tool: tool})
		if err != nil {
			t.Fatal(err)
		}
		if pol.User != "user-a" {
			t.Errorf("%q's claim resolved naming %q, want user-a", tool, pol.User)
		}
	}
	if len(asked) != 3 || asked[0] != "p1@https://otel.example" {
		t.Errorf("asked who parts name for %q, want each claim's, for its own ingest", asked)
	}
}
