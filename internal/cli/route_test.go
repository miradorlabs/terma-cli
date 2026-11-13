package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const ownOrg = "33333333-3333-4333-8333-333333333333"

// planState is the plan's scenario list: Acme Platform (github.com/acme/*), Beta Labs'
// Contractors (github.com/clientco/app), Personal (everything); github.com/acme bound to
// Acme, github.com/clientco/app to Beta Labs.
func planState(t *testing.T) (routing.State, teamNames) {
	t.Helper()
	const authURL = "https://auth.example.test"
	team := func(org, id string, personal bool, repos ...string) routing.Team {
		pol := config.Policy{Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: time.Now().UTC(),
			OrganizationID: org, AuthURL: authURL, TeamID: id, Wildcards: true}
		if personal {
			pol.Mode = config.ModeGlobal
		}
		return routing.Team{OrganizationID: org, AuthURL: authURL, TeamID: id, Personal: personal, Policy: pol}
	}
	var st routing.State
	for _, tm := range []routing.Team{
		team(orgA().ID, acmeWeb, false, "github.com/acme/*"),
		team(orgB().ID, betaCore, false, "github.com/clientco/app"),
		team(ownOrg, "pppppppp-0000-4000-8000-000000000001", true),
	} {
		if _, err := st.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Bind("github.com/acme", orgA().ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.Bind("github.com/clientco/app", orgB().ID, false); err != nil {
		t.Fatal(err)
	}
	names := teamNames{
		Teams: map[string]string{acmeWeb: "Platform", betaCore: "Contractors", "pppppppp-0000-4000-8000-000000000001": "Personal"},
		Orgs:  map[string]string{orgA().ID: "Acme", orgB().ID: "Beta Labs", ownOrg: "Personal"},
	}
	return st, names
}

func explain(st routing.State, names teamNames, repo string) string {
	loc := routing.Location{Kind: routing.OutsideGit}
	if repo != "" {
		loc = routing.Location{Kind: routing.Repository, Repository: repo}
	}
	var b bytes.Buffer
	explainRoute(&b, st, names, loc, time.Now())
	return b.String()
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("route explain lacks %q:\n%s", w, out)
		}
	}
}

// route explain names the team, the entry and the binding that decided, from the plan's
// scenarios.
func TestRouteExplainNamesTheTeamEntryAndBinding(t *testing.T) {
	st, names := planState(t)
	wantLines(t, explain(st, names, "git@github.com:Acme/api.git"),
		"Goes to:   Acme Platform", "Entry:     github.com/acme/*", "Binding:   github.com/acme is bound to Acme: only it decides")
	wantLines(t, explain(st, names, "https://github.com/clientco/app"), "Goes to:   Beta Labs Contractors", "Entry:     github.com/clientco/app")
	wantLines(t, explain(st, names, "github.com/dawson/side"), "Goes to:   Personal\n", "nothing binds it to an organization")
	wantLines(t, explain(st, names, ""), "Location:  outside any repository", "Goes to:   Personal\n")
}

// A wildcard needs its owner bound: an undecided owner holds what it matches, and route
// explain says how to decide.
func TestRouteExplainSaysAWildcardNeedsItsOwnerBound(t *testing.T) {
	st, names := planState(t)
	if err := st.Unbind("github.com/acme"); err != nil {
		t.Fatal(err)
	}
	wantLines(t, explain(st, names, "github.com/acme/api"),
		"held, then dropped: Acme Platform's entry needs a binding", "github.com/acme is undecided: `terma teams --bind github.com/acme=<organization>`")
}

// A repository bound to an organization that lists it only through a wildcard whose owner
// is bound elsewhere collects nothing, and route explain says why (the council's build note).
func TestRouteExplainSaysWhyARepositoryBindingAloneCollectsNothing(t *testing.T) {
	st, names := planState(t)
	st.Teams[1].Policy.Repositories = []string{"github.com/acme/*"}
	if err := st.Bind("github.com/acme/api", orgB().ID, true); err != nil {
		t.Fatal(err)
	}
	wantLines(t, explain(st, names, "github.com/acme/api"),
		"Goes to:   nothing leaves this machine",
		"Acme Platform lists github.com/acme/*, but github.com/acme/api is bound to Beta Labs",
		"so binding github.com/acme/api alone to Beta Labs collects nothing here")
}

// Personal collecting under an owner some of whose repositories are bound to an
// organization is said.
func TestRouteExplainSaysPersonalCollectsUnderAPartlyBoundOwner(t *testing.T) {
	st, names := planState(t)
	wantLines(t, explain(st, names, "github.com/clientco/other"),
		"Goes to:   Personal", "other repositories under github.com/clientco are bound to an organization: github.com/clientco/app (Beta Labs)")
}

// The Unknown rule: personal gets an unknown record only as the only ticked team, with
// nothing bound to an organization, and route explain says which part fails.
func TestRouteExplainStatesTheUnknownRule(t *testing.T) {
	st, names := planState(t)
	wantLines(t, explain(st, names, ""), "Unknown:   a record whose location is unknown is held, then dropped: personal is not the only ticked team")
	st.Deselect(st.Teams[0].EnrollmentID)
	st.Deselect(st.Teams[0].EnrollmentID)
	wantLines(t, explain(st, names, ""), "something is bound to an organization")
	st.Bindings = nil
	wantLines(t, explain(st, names, ""), "Unknown:   a record whose location is unknown goes to personal")
}

// End to end: the folder's origin, read as a hook reads it; and a machine not yet migrated
// is answered by its one team, as it routes today.
func TestRouteExplainReadsTheFoldersOrigin(t *testing.T) {
	repo := gitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := runTerma(t, "route", "explain", repo)
	if err != nil || !strings.Contains(out, "Location:  github.com/acme/app (a repository)") || !strings.Contains(out, "routes by its one team") {
		t.Fatalf("route explain before migration (%v):\n%s", err, out)
	}
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeWeb, false, "github.com/acme/app"))
		if err != nil {
			return err
		}
		return s.Bind("github.com/acme/app", orgA().ID, false)
	})
	t.Chdir(repo)
	out, err = runTerma(t, "route", "explain")
	if err != nil || !strings.Contains(out, "Goes to:   "+acmeWeb) || !strings.Contains(out, "Entry:     github.com/acme/app") {
		t.Fatalf("route explain once migrated (%v):\n%s", err, out)
	}
}
