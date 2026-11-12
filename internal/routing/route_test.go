package routing

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// team is a selected team with a policy validated an hour before now.
func team(org, id string, entries ...string) Team {
	return Team{OrganizationID: org, TeamID: id, Policy: config.Policy{
		Mode: config.ModeRepo, TeamID: id, Repositories: entries, Wildcards: true, Revision: 1,
		FetchedAt: now.Add(-time.Hour),
	}}
}

func personal(entries ...string) Team {
	t := team("me", "personal", entries...)
	t.Personal = true
	if len(entries) == 0 {
		t.Policy.Mode = config.ModeGlobal
	}
	return t
}

// fixture is the plan's scenario team list: Acme Platform, ClientCo Contractors and
// Personal, with github.com/acme bound to Acme and github.com/clientco/app to ClientCo.
func fixture(t *testing.T, teams ...Team) *State {
	t.Helper()
	if teams == nil {
		teams = []Team{team("acme", "acme-platform", "github.com/acme/*"), team("clientco", "clientco-contractors", "github.com/clientco/app"), personal()}
	}
	s := &State{Profile: "default"}
	for _, tm := range teams {
		if _, err := s.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	must(t, s.Bind("github.com/acme", "acme", false))
	must(t, s.Bind("github.com/clientco/app", "clientco", false))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// enrollment is the enrollment of the selected team id.
func enrollment(t *testing.T, s *State, id string) string {
	t.Helper()
	for _, tm := range s.Teams {
		if tm.TeamID == id {
			return tm.EnrollmentID
		}
	}
	t.Fatalf("team %s is not selected", id)
	return ""
}

func repo(r string) Location { return Location{Kind: Repository, Repository: r} }

// expect routes loc and checks the outcome and, unless want is "", the team.
func expect(t *testing.T, s *State, loc Location, outcome Outcome, want string) {
	t.Helper()
	d := Route(loc, *s, now)
	got := ""
	if d.EnrollmentID != "" {
		got = s.team(d.EnrollmentID).TeamID
	}
	if d.Outcome != outcome || got != want {
		t.Errorf("Route(%+v) = %s to %q (%s), want %s to %q", loc, d.Outcome, got, d.Reason, outcome, want)
	}
}

// The plan's scenario table, row by row.
func TestRouteScenarios(t *testing.T) {
	t.Parallel()
	t.Run("matched", func(t *testing.T) {
		s := fixture(t)
		expect(t, s, repo("github.com/acme/api"), Send, "acme-platform")
		expect(t, s, repo("github.com/clientco/app"), Send, "clientco-contractors")
		expect(t, s, repo("github.com/clientco/other"), Send, "personal")
		expect(t, s, repo("github.com/dawson/side"), Send, "personal")
		expect(t, s, Location{Kind: OutsideGit}, Send, "personal")
	})
	t.Run("a bound owner's unlisted repository goes nowhere", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/api"), personal())
		must(t, s.Bind("github.com/acme", "acme", false))
		expect(t, s, repo("github.com/acme/secret"), Drop, "")
	})
	t.Run("an expired, suspended or refused team holds, never passes on", func(t *testing.T) {
		for _, block := range []func(*Team){
			func(tm *Team) { tm.Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) },
			func(tm *Team) { tm.Refusals = 1 },
			func(tm *Team) { tm.Refusals = 2 },
			func(tm *Team) { tm.SignInNeeded = true },
		} {
			s := fixture(t)
			block(&s.Teams[0])
			expect(t, s, repo("github.com/acme/api"), Hold, "acme-platform")
		}
	})
	t.Run("unticking leaves the binding, so nothing until it is unbound", func(t *testing.T) {
		s := fixture(t)
		s.Deselect(enrollment(t, s, "acme-platform"))
		expect(t, s, repo("github.com/acme/api"), Drop, "")
		must(t, s.Unbind("github.com/acme"))
		expect(t, s, repo("github.com/acme/api"), Send, "personal")
	})
	t.Run("another organization listing a bound owner changes nothing", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = append(s.Teams[1].Policy.Repositories, "github.com/acme/*")
		expect(t, s, repo("github.com/acme/api"), Send, "acme-platform")
	})
	t.Run("an exact entry under an undecided owner binds only its repository", func(t *testing.T) {
		s := fixture(t)
		s.Teams[0].Policy.Repositories = append(s.Teams[0].Policy.Repositories, "github.com/torvalds/linux")
		expect(t, s, repo("github.com/torvalds/linux"), Hold, "acme-platform")
		expect(t, s, repo("github.com/torvalds/other"), Send, "personal")
		must(t, s.Bind("github.com/torvalds/linux", "acme", false))
		expect(t, s, repo("github.com/torvalds/linux"), Send, "acme-platform")
		expect(t, s, repo("github.com/torvalds/other"), Send, "personal")
	})
	t.Run("an organization claiming the developer's owner blocks until refused", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = append(s.Teams[1].Policy.Repositories, "github.com/dawson/*")
		expect(t, s, repo("github.com/dawson/side"), Hold, "clientco-contractors")
		must(t, s.Refuse("github.com/dawson"))
		expect(t, s, repo("github.com/dawson/side"), Send, "personal")
	})
	t.Run("an exact entry beats a wildcard within an organization", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/*"), team("acme", "acme-mobile", "github.com/acme/api"), personal())
		expect(t, s, repo("github.com/acme/api"), Send, "acme-mobile")
		expect(t, s, repo("github.com/acme/web"), Send, "acme-platform")
	})
	t.Run("a mixed run sends nothing", func(t *testing.T) {
		expect(t, fixture(t), Location{Kind: Mixed}, Drop, "")
	})
	t.Run("a narrowed personal policy", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/*"), personal("github.com/dawson/terma"))
		expect(t, s, repo("github.com/dawson/side"), Drop, "")
		expect(t, s, repo("github.com/dawson/terma"), Send, "personal")
		expect(t, s, Location{Kind: OutsideGit}, Drop, "")
	})
	t.Run("nothing ticked matches", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/*"))
		expect(t, s, repo("github.com/dawson/side"), Drop, "")
		expect(t, s, Location{Kind: OutsideGit}, Drop, "")
	})
}

// An unknown location reaches personal only as the only team, Active, collecting
// everything, with nothing bound to an organization, and eligible when it arrived.
func TestUnknownGoesToPersonalOnlyWhenNothingElseCouldClaimIt(t *testing.T) {
	t.Parallel()
	unknown := Location{Kind: Unknown}
	expect(t, fixture(t), unknown, Hold, "")

	alone := &State{Profile: "default"}
	if _, err := alone.Select(personal()); err != nil {
		t.Fatal(err)
	}
	expect(t, alone, unknown, Send, "personal")
	expect(t, alone, Location{Kind: Unknown, Narrow: true}, Hold, "")

	must(t, alone.Bind("github.com/acme", "acme", false))
	expect(t, alone, unknown, Hold, "")
	must(t, alone.Refuse("github.com/acme"))
	expect(t, alone, unknown, Send, "personal")

	alone.Teams[0].SignInNeeded = true
	expect(t, alone, unknown, Hold, "")
}

// The council's binding cases: authority and consent are separate checks, and an
// organization's entry never overrides a decision the developer made.
func TestAuthorityAndConsent(t *testing.T) {
	t.Parallel()
	t.Run("a refused repository under a bound owner", func(t *testing.T) {
		s := fixture(t)
		must(t, s.Refuse("github.com/acme/api"))
		expect(t, s, repo("github.com/acme/api"), Send, "personal")
		expect(t, s, repo("github.com/acme/web"), Send, "acme-platform")
	})
	t.Run("a repository bound to another organization than its owner", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = append(s.Teams[1].Policy.Repositories, "github.com/acme/shared")
		if err := s.Bind("github.com/acme/shared", "clientco", false); err == nil {
			t.Fatal("bound a repository against its owner's binding without naming the conflict")
		}
		must(t, s.Bind("github.com/acme/shared", "clientco", true))
		expect(t, s, repo("github.com/acme/shared"), Send, "clientco-contractors")
		expect(t, s, repo("github.com/acme/api"), Send, "acme-platform")
	})
	t.Run("a wildcard needs its owner's binding, not a repository's", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = []string{"github.com/acme/*"}
		must(t, s.Bind("github.com/acme/shared", "clientco", true))
		expect(t, s, repo("github.com/acme/shared"), Drop, "")
	})
	t.Run("unbinding a repository reveals its owner's decision", func(t *testing.T) {
		s := fixture(t)
		must(t, s.Refuse("github.com/acme/api"))
		must(t, s.Unbind("github.com/acme/api"))
		expect(t, s, repo("github.com/acme/api"), Send, "acme-platform")
	})
	t.Run("pending blocks only what its entry matches", func(t *testing.T) {
		s := fixture(t, team("clientco", "clientco-contractors", "github.com/acme/api"), personal())
		must(t, s.Unbind("github.com/acme"))
		expect(t, s, repo("github.com/acme/api"), Hold, "clientco-contractors")
		expect(t, s, repo("github.com/acme/web"), Send, "personal")
	})
	t.Run("a blocked team cannot block another organization's repository", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = []string{"github.com/acme/*"}
		s.Teams[1].Refusals = 2
		expect(t, s, repo("github.com/acme/api"), Send, "acme-platform")
	})
	t.Run("GitLab subgroups: the more fixed segments, the earlier", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-all", "gitlab.com/acme/*"), team("acme", "acme-platform", "gitlab.com/acme/platform/*"), personal())
		must(t, s.Bind("gitlab.com/acme", "acme", false))
		expect(t, s, repo("gitlab.com/acme/platform/api"), Send, "acme-platform")
		expect(t, s, repo("gitlab.com/acme/other"), Send, "acme-all")
	})
	t.Run("one normaliser for origins, entries and bindings", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "GitHub.com/Acme/*"), personal())
		must(t, s.Bind("GITHUB.COM/ACME/", "acme", false))
		expect(t, s, repo("github.com/ACME/Api"), Send, "acme-platform")
	})
	t.Run("an organization's collect-everything policy collects nothing", func(t *testing.T) {
		s := fixture(t)
		_, err := s.ApplyRefresh(Refresh{EnrollmentID: s.Teams[0].EnrollmentID, Policy: &config.Policy{
			Mode: config.ModeGlobal, TeamID: "acme-platform", Revision: 2, FetchedAt: now,
		}})
		must(t, err)
		expect(t, s, repo("github.com/acme/api"), Drop, "")
		expect(t, s, Location{Kind: OutsideGit}, Send, "personal")
	})
}

// Which organization a location belongs to, for telling when a run crosses.
func TestOrganizationOf(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	for _, c := range []struct {
		loc     Location
		org     string
		pending bool
		ok      bool
	}{
		{repo("github.com/acme/api"), "acme", false, true},
		{repo("github.com/acme/unlisted-but-bound"), "acme", false, true},
		{repo("github.com/dawson/side"), "me", false, true},
		{Location{Kind: OutsideGit}, "me", false, true},
	} {
		org, pending, ok := OrganizationOf(c.loc, *s, now)
		if org != c.org || pending != c.pending || ok != c.ok {
			t.Errorf("OrganizationOf(%+v) = %q, %v, %v; want %q, %v, %v", c.loc, org, pending, ok, c.org, c.pending, c.ok)
		}
	}
	s.Teams[1].Policy.Repositories = append(s.Teams[1].Policy.Repositories, "github.com/dawson/*")
	if _, pending, ok := OrganizationOf(repo("github.com/dawson/side"), *s, now); !pending || !ok {
		t.Error("a pending repository did not report pending")
	}
	narrowed := fixture(t, team("acme", "acme-platform", "github.com/acme/*"), personal("github.com/dawson/terma"))
	if _, _, ok := OrganizationOf(repo("github.com/dawson/side"), *narrowed, now); ok {
		t.Error("a location nothing collects or binds belongs to an organization")
	}
}
