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
	if p.Global() || !p.CollectsNothing || p.IncludePrompts || p.IncludeToolContent || !p.AppliesTo("org", "https://auth") || p.AppliesTo("other", "https://auth") {
		t.Fatalf("NoPolicy = %+v", p)
	}
}

// A policy expires MaxPolicyAge after it was validated; one never fetched has no age.
func TestPolicyExpires(t *testing.T) {
	now := time.Now()
	if (Policy{FetchedAt: now.Add(-MaxPolicyAge + time.Minute)}).Expired(now) {
		t.Fatal("expired before MaxPolicyAge")
	}
	if !(Policy{FetchedAt: now.Add(-MaxPolicyAge - time.Minute)}).Expired(now) {
		t.Fatal("still valid after MaxPolicyAge")
	}
	if (Policy{}).Expired(now) {
		t.Fatal("a policy never fetched reports an age")
	}
}

// Content is the team's policy alone: path exclusions and a policy not validated
// withhold all of it.
func TestContentIsTheTeamPolicys(t *testing.T) {
	for _, c := range []struct {
		name          string
		pol           Policy
		prompts, tool bool
	}{
		{"both on", Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true}, true, true},
		{"both off", Policy{Mode: ModeRepo}, false, false},
		{"prompts only", Policy{Mode: ModeRepo, IncludePrompts: true}, true, false},
		{"tool content only", Policy{Mode: ModeRepo, IncludeToolContent: true}, false, true},
		{"paths excluded", Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true, ExcludePaths: []string{".env"}}, false, false},
		{"not validated", NoPolicy("", ""), false, false},
	} {
		if prompts, tool := c.pol.Content(); prompts != c.prompts || tool != c.tool {
			t.Errorf("%s: prompts=%v tool content=%v, want %v %v", c.name, prompts, tool, c.prompts, c.tool)
		}
	}
}
