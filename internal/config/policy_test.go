package config

import (
	"testing"
	"time"
)

// A policy is validated once fetched for a team; the offline stub stands in for the team,
// never for the fetch.
func TestPolicyValidated(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	fetched := Policy{TeamID: "t", FetchedAt: time.Now()}
	if !fetched.Validated() {
		t.Fatal("a fetched team policy is not validated")
	}
	if (Policy{TeamID: "t"}).Validated() || (Policy{FetchedAt: time.Now()}).Validated() {
		t.Fatal("an unfetched or teamless policy was validated")
	}
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo"}`)
	if !(Policy{FetchedAt: time.Now()}).Validated() || (Policy{}).Validated() {
		t.Fatal("the stub must stand in for the team only")
	}
}

// Before validation nothing is collected, and the default names its login.
func TestNoPolicyCollectsNothing(t *testing.T) {
	p := NoPolicy("org", "https://auth")
	if p.Global() || p.AllowsSignal("logs") || p.IncludePrompts || p.IncludeToolContent || !p.AppliesTo("org", "https://auth") || p.AppliesTo("other", "https://auth") {
		t.Fatalf("NoPolicy = %+v", p)
	}
}
