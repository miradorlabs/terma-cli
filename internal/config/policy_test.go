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
	t.Parallel()
	p := NoPolicy("org", "https://auth")
	if p.Global() || !p.CollectsNothing || p.IncludePrompts || p.IncludeToolContent || !p.AppliesTo("org", "https://auth") || p.AppliesTo("other", "https://auth") {
		t.Fatalf("NoPolicy = %+v", p)
	}
}

// A policy expires MaxPolicyAge after it was validated; one never fetched has no age.
func TestPolicyExpires(t *testing.T) {
	t.Parallel()
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

// Content is the team's policy alone: a policy not validated withholds all of it.
func TestContentIsTheTeamPolicys(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		pol           Policy
		prompts, tool bool
	}{
		{"both on", Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true}, true, true},
		{"both off", Policy{Mode: ModeRepo}, false, false},
		{"prompts only", Policy{Mode: ModeRepo, IncludePrompts: true}, true, false},
		{"tool content only", Policy{Mode: ModeRepo, IncludeToolContent: true}, false, true},
		{"not validated", NoPolicy("", ""), false, false},
	} {
		if prompts, tool := c.pol.Content(); prompts != c.prompts || tool != c.tool {
			t.Errorf("%s: prompts=%v tool content=%v, want %v %v", c.name, prompts, tool, c.prompts, c.tool)
		}
	}
}

// Hooks apply a policy once validated, NoPolicy before; a list of no usable entry admits none.
func TestPolicyInForceAndAdmitsNone(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	fetched := Policy{Mode: ModeRepo, Repositories: []string{"github.com/acme/app"}, TeamID: "t", FetchedAt: time.Now()}
	if got := fetched.InForce("org", "a"); got.CollectsNothing || got.AdmitsNone() {
		t.Fatalf("a validated policy was not in force: %+v", got)
	}
	if got := (Policy{Mode: ModeGlobal}).InForce("org", "a"); got.Global() || !got.CollectsNothing || !got.AdmitsNone() {
		t.Fatalf("an unvalidated policy was in force: %+v", got)
	}
	blank := fetched
	blank.Repositories = []string{" ", "/", "app", "github.com/acme"}
	global := Policy{Mode: ModeGlobal, TeamID: "t", FetchedAt: time.Now()}
	if !blank.AdmitsNone() || global.AdmitsNone() {
		t.Fatal("AdmitsNone misjudged a blank list or global mode")
	}
}

// A validated policy is stale PolicyStaleAfter after its fetch; one never validated is not.
func TestPolicyStale(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	now := time.Now()
	if (Policy{TeamID: "t", FetchedAt: now.Add(-PolicyStaleAfter + time.Second)}).Stale(now) {
		t.Fatal("stale before PolicyStaleAfter")
	}
	if !(Policy{TeamID: "t", FetchedAt: now.Add(-PolicyStaleAfter - time.Second)}).Stale(now) {
		t.Fatal("fresh after PolicyStaleAfter")
	}
	if (Policy{FetchedAt: now.Add(-time.Hour)}).Stale(now) {
		t.Fatal("a policy never validated is stale")
	}
}

// A policy's team is its own, else global mode's default project.
func TestPolicyTeam(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		p    Policy
		want string
	}{
		{Policy{TeamID: "t", Mode: ModeGlobal, DefaultProjectID: "d"}, "t"},
		{Policy{Mode: ModeGlobal, DefaultProjectID: "d"}, "d"},
		{Policy{Mode: ModeRepo, DefaultProjectID: "d"}, ""},
	} {
		if got := c.p.Team(); got != c.want {
			t.Errorf("%+v.Team() = %q, want %q", c.p, got, c.want)
		}
	}
}
