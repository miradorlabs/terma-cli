package config

import (
	"os"
	"path/filepath"
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

// A repository goes to the first team whose list names it, across every policy the
// machine collects under; a global policy takes what no list places.
func TestPoliciesAdmitByAnyListBeforeGlobal(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	now := time.Now()
	a := Policy{Mode: ModeRepo, Repositories: []string{"github.com/acme/app"}, TeamID: "ta", OrganizationID: "org_a", FetchedAt: now}
	b := Policy{Mode: ModeRepo, Repositories: []string{"github.com/beta/site"}, TeamID: "tb", OrganizationID: "org_b", FetchedAt: now}
	g := Policy{Mode: ModeGlobal, TeamID: "tg", DefaultProjectID: "tg", OrganizationID: "org_a", FetchedAt: now}
	ps := Policies{a, b}
	if got, ok := ps.Admitting(Repository{Origin: "github.com/beta/site"}); !ok || got.TeamID != "tb" {
		t.Fatalf("another organization's listing did not admit: %+v, %v", got, ok)
	}
	if _, ok := ps.Admitting(Repository{Origin: "github.com/me/personal"}); ok {
		t.Fatal("an unlisted repository was admitted with no global policy")
	}
	if _, ok := ps.Global(); ok || ps.Validated() != true {
		t.Fatal("a collection of repository-mode policies has no global one")
	}
	ps = Policies{g, a, b}
	if got, ok := ps.Admitting(Repository{Origin: "github.com/beta/site"}); !ok || got.TeamID != "tb" {
		t.Fatalf("a listing lost to the global policy: %+v, %v", got, ok)
	}
	if got, ok := ps.Admitting(Repository{Origin: "github.com/me/personal"}); !ok || got.TeamID != "tg" {
		t.Fatalf("the global policy did not take the unlisted repository: %+v, %v", got, ok)
	}
	if got, ok := ps.Global(); !ok || got.TeamID != "tg" {
		t.Fatalf("Global = %+v, %v", got, ok)
	}
	if (Policies{}).Validated() {
		t.Fatal("no policy is validated")
	}
}

// Label names a team for a person from what the policy stored, falling back to ids.
func TestPolicyLabel(t *testing.T) {
	for _, tc := range []struct {
		pol  Policy
		want string
	}{
		{Policy{TeamID: "t1", TeamName: "Platform", OrganizationID: "o1", OrganizationName: "Acme"}, "team Platform of Acme"},
		{Policy{TeamID: "t1", OrganizationID: "o1"}, "team t1 of o1"},
		{Policy{TeamID: "t1"}, "team t1"},
		{Policy{OrganizationName: "Acme"}, "Acme"},
	} {
		if got := tc.pol.Label(); got != tc.want {
			t.Errorf("%+v: Label = %q, want %q", tc.pol, got, tc.want)
		}
	}
}

// ListPolicies reads every team's stored policy and skips what is not one.
func TestListPoliciesReadsEveryTeam(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	dir := t.TempDir()
	if got, err := ListPolicies(dir); err != nil || len(got) != 0 {
		t.Fatalf("empty state dir: %v, %v", got, err)
	}
	now := time.Now()
	for _, team := range []string{"tb", "ta"} {
		if err := WritePolicy(dir, Policy{Mode: ModeRepo, TeamID: team, FetchedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, PoliciesDir, "junk.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, PoliciesDir, "refreshed"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ListPolicies(dir)
	if err != nil || len(got) != 2 || got[0].TeamID != "ta" || got[1].TeamID != "tb" {
		t.Fatalf("ListPolicies = %+v, %v", got, err)
	}
}
