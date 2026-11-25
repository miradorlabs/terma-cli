package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A sign-in as a person makes the login the profile's credential again, not the server key
// an earlier setup stored, and keeps the organization and team its own account chose.
func TestApplyLoginClearsTheServerKeySignIn(t *testing.T) {
	p := &config.Profile{OrganizationID: "org", Team: "team", ServerKeySignIn: true}
	applyLogin(p, &config.Config{Environment: config.EnvProd}, "")
	if p.ServerKeySignIn {
		t.Fatalf("profile after a sign-in = %+v", p)
	}
	p.OrganizationID, p.Team = "org", "team"
	applyLogin(p, &config.Config{Environment: config.EnvProd}, "")
	if p.Team != "team" || p.OrganizationID != "org" {
		t.Fatalf("a sign-in as the same person dropped their own choice: %+v", p)
	}
}

// A sign-in under TERMA_ENV pins its environment, so a hook started without it resolves
// to the same backend; one in production clears the pin.
func TestApplyLoginPinsEnvironment(t *testing.T) {
	p := &config.Profile{}
	applyLogin(p, &config.Config{Environment: config.EnvDev}, "")
	if p.Environment != config.EnvDev {
		t.Fatalf("environment = %q, want %q", p.Environment, config.EnvDev)
	}
	applyLogin(p, &config.Config{Environment: config.EnvProd}, "")
	if p.Environment != "" {
		t.Fatalf("environment = %q after a production sign-in, want none", p.Environment)
	}
}

// A server key's team was chosen under no person's sign-in: a login afterwards, even as the
// person signed in before the key, keeps neither its organization nor its team (#163).
func TestALoginAfterAServerKeyDropsTheKeysTeam(t *testing.T) {
	const account = "aaaaaaaaaaaaaaaa"
	p := &config.Profile{Account: account, OrganizationID: "org-a", Team: "team-t"}
	applyServerKey(p, &config.Config{Environment: config.EnvProd, OrganizationID: "org-x", Team: "team-x"})
	if p.Account != "" {
		t.Fatalf("a server key setup kept the person's account %q", p.Account)
	}
	applyLogin(p, &config.Config{Environment: config.EnvProd}, account)
	if p.ServerKeySignIn || p.OrganizationID != "" || p.Team != "" || p.Account != account {
		t.Fatalf("profile after signing in again = %+v; want the key's organization and team cleared", p)
	}
}

// A server-key profile saved before applyServerKey unpaired it still names the person
// signed in before the key: signing in again as that person keeps neither the key's
// organization nor its team (#163).
func TestALoginOverAServerKeyProfileSavedPairedDropsTheKeysTeam(t *testing.T) {
	const account = "aaaaaaaaaaaaaaaa"
	p := &config.Profile{Account: account, OrganizationID: "org-x", Team: "team-x", ServerKeySignIn: true}
	applyLogin(p, &config.Config{Environment: config.EnvProd}, account)
	if p.ServerKeySignIn || p.OrganizationID != "" || p.Team != "" || p.Account != account {
		t.Fatalf("profile after signing in again = %+v; want the key's organization and team cleared", p)
	}
}
