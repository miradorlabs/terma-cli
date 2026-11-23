package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A sign-in as a person makes the login the profile's credential again, not the server key
// an earlier setup stored, and keeps the organization and team chosen.
func TestApplyLoginClearsTheServerKeySignIn(t *testing.T) {
	p := &config.Profile{OrganizationID: "org", Team: "team", ServerKeySignIn: true}
	applyLogin(p, &config.Config{Environment: config.EnvProd}, "")
	if p.ServerKeySignIn || p.Team != "team" || p.OrganizationID != "org" {
		t.Fatalf("profile after a sign-in = %+v", p)
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
