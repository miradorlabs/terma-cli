package routing

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// authURL is the sign-in environment every test team and its policies were fetched in.
const authURL = "https://auth.example"

// testAccount and bobAccount are valid 16-hex account keys for routing state tests.
const (
	testAccount = "0123456789abcdef"
	bobAccount  = "fedcba9876543210"
)

// team is a selected team with a policy validated an hour before now.
func team(org, id string, entries ...string) Team {
	return Team{OrganizationID: org, AuthURL: authURL, TeamID: id, Policy: config.Policy{
		Mode: config.ModeRepo, TeamID: id, OrganizationID: org, AuthURL: authURL, Repositories: entries, Wildcards: true, Revision: 1,
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

// fixture is the scenario team list: Acme Platform, ClientCo Contractors and Personal.
func fixture(t *testing.T, teams ...Team) *State {
	t.Helper()
	if teams == nil {
		teams = []Team{team("acme", "acme-platform", "github.com/acme/*"), team("clientco", "clientco-contractors", "github.com/clientco/app"), personal()}
	}
	s := &State{Profile: "default", Account: testAccount}
	for _, tm := range teams {
		if _, err := s.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// updated is tm with its policy last updated at.
func updated(tm Team, at time.Time) Team {
	tm.Policy.UpdatedAt = at
	return tm
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
	d := NewRouter(*s).Route(loc, now)
	got := ""
	if d.EnrollmentID != "" {
		got = s.team(d.EnrollmentID).TeamID
	}
	if d.Outcome != outcome || got != want {
		t.Errorf("Route(%+v) = %s to %q (%s), want %s to %q", loc, d.Outcome, got, d.Reason, outcome, want)
	}
}

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
	t.Run("an expired, suspended or signed-out team holds, never passes on", func(t *testing.T) {
		for _, block := range []func(*Team){
			func(tm *Team) { tm.Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) },
			func(tm *Team) { tm.Refusals = 1 },
			func(tm *Team) { tm.SignInNeeded = true },
		} {
			s := fixture(t)
			block(&s.Teams[0])
			expect(t, s, repo("github.com/acme/api"), Hold, "acme-platform")
		}
	})
	t.Run("unticking a team hands its repositories to personal", func(t *testing.T) {
		s := fixture(t)
		s.Deselect(enrollment(t, s, "acme-platform"))
		expect(t, s, repo("github.com/acme/api"), Send, "personal")
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
	t.Run("an organization's collect-everything policy collects nothing", func(t *testing.T) {
		s := fixture(t)
		_, err := s.ApplyRefresh(Refresh{EnrollmentID: s.Teams[0].EnrollmentID, Policy: &config.Policy{
			Mode: config.ModeGlobal, TeamID: "acme-platform", OrganizationID: "acme", AuthURL: authURL, Revision: 2, FetchedAt: now,
		}})
		must(t, err)
		expect(t, s, repo("github.com/acme/api"), Send, "personal")
		expect(t, s, Location{Kind: OutsideGit}, Send, "personal")
	})
}

// When several organizations' policies match a repository, the most recently updated
// policy wins it, whatever the entries' specificity; equal ages keep the order the teams
// were ticked in.
func TestNewestPolicyWins(t *testing.T) {
	t.Parallel()
	older, newer := now.Add(-48*time.Hour), now.Add(-time.Hour)
	t.Run("the newer policy wins an overlap", func(t *testing.T) {
		s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), newer),
			updated(team("clientco", "clientco-contractors", "github.com/shared/*"), older), personal())
		expect(t, s, repo("github.com/shared/api"), Send, "acme-platform")
	})
	t.Run("age beats specificity", func(t *testing.T) {
		s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), newer),
			updated(team("clientco", "clientco-contractors", "github.com/shared/api"), older), personal())
		expect(t, s, repo("github.com/shared/api"), Send, "acme-platform")
	})
	t.Run("updating a policy moves it ahead of the others", func(t *testing.T) {
		s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), older),
			updated(team("clientco", "clientco-contractors", "github.com/shared/*"), newer), personal())
		expect(t, s, repo("github.com/shared/api"), Send, "clientco-contractors")
		s.Teams[0].Policy.UpdatedAt = now
		expect(t, s, repo("github.com/shared/api"), Send, "acme-platform")
	})
	t.Run("a policy of unknown age never takes a dated one's repository", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/shared/*"),
			updated(team("clientco", "clientco-contractors", "github.com/shared/*"), older), personal())
		expect(t, s, repo("github.com/shared/api"), Send, "clientco-contractors")
		if o := Overlaps(*s); len(o) != 1 || o[0].Winner.TeamID != "clientco-contractors" {
			t.Fatalf("Overlaps = %+v, want clientco-contractors to win", o)
		}
	})
	t.Run("equal ages keep the order the teams were ticked in", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-first", "github.com/acme/*"), team("acme", "acme-second", "github.com/acme/*"), personal())
		expect(t, s, repo("github.com/acme/api"), Send, "acme-first")
	})
	t.Run("an expired, signed-out or suspended winner holds instead of passing on", func(t *testing.T) {
		for _, block := range []func(*Team){
			func(tm *Team) { tm.Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) },
			func(tm *Team) { tm.SignInNeeded = true },
			func(tm *Team) { tm.Refusals = 1 },
		} {
			s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), newer),
				updated(team("clientco", "clientco-contractors", "github.com/shared/*"), older), personal())
			block(&s.Teams[0])
			expect(t, s, repo("github.com/shared/api"), Hold, "acme-platform")
		}
	})
	t.Run("a refused winner is passed over for the next policy, then personal", func(t *testing.T) {
		s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), newer),
			updated(team("clientco", "clientco-contractors", "github.com/shared/*"), older), personal())
		s.Teams[0].Refusals = 2
		expect(t, s, repo("github.com/shared/api"), Send, "clientco-contractors")
		if o := Overlaps(*s); len(o) != 0 {
			t.Errorf("Overlaps names a refused team: %+v", o)
		}
		s.Teams[1].Refusals = 2
		expect(t, s, repo("github.com/shared/api"), Send, "personal")
	})
	t.Run("a team that stops listing a repository gives it up", func(t *testing.T) {
		s := fixture(t, updated(team("acme", "acme-platform", "github.com/shared/*"), newer),
			updated(team("clientco", "clientco-contractors", "github.com/shared/*"), older), personal())
		s.Teams[0].Policy.Repositories = []string{"github.com/acme/*"}
		expect(t, s, repo("github.com/shared/api"), Send, "clientco-contractors")
	})
	t.Run("GitLab subgroups match within a team", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "gitlab.com/acme/platform/*"), personal())
		expect(t, s, repo("gitlab.com/acme/platform/api"), Send, "acme-platform")
		expect(t, s, repo("gitlab.com/acme/other"), Send, "personal")
	})
	t.Run("one normaliser for origins and entries", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "GitHub.com/Acme/*"), personal())
		expect(t, s, repo("github.com/ACME/Api"), Send, "acme-platform")
	})
}

// An unknown location reaches personal only as the only team, Active and collecting
// everything, and eligible when it arrived.
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

	alone.Teams[0].SignInNeeded = true
	expect(t, alone, unknown, Hold, "")
}

// Which organization a location belongs to, for telling when a run crosses.
func TestOrganizationOf(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	for _, c := range []struct {
		loc Location
		org string
		ok  bool
	}{
		{repo("github.com/acme/api"), "acme", true},
		{repo("github.com/dawson/side"), "me", true},
		{Location{Kind: OutsideGit}, "me", true},
	} {
		org, ok := NewRouter(*s).OrganizationOf(c.loc, now)
		if org != c.org || ok != c.ok {
			t.Errorf("OrganizationOf(%+v) = %q, %v; want %q, %v", c.loc, org, ok, c.org, c.ok)
		}
	}
	narrowed := fixture(t, team("acme", "acme-platform", "github.com/acme/*"), personal("github.com/dawson/terma"))
	if _, ok := NewRouter(*narrowed).OrganizationOf(repo("github.com/dawson/side"), now); ok {
		t.Error("a location nothing collects belongs to an organization")
	}
}

// Overlaps names every entry two organizations both cover, and the team Route gives it.
func TestOverlaps(t *testing.T) {
	t.Parallel()
	older, newer := now.Add(-48*time.Hour), now.Add(-time.Hour)
	s := fixture(t,
		updated(team("acme", "acme-platform", "github.com/shared/*", "github.com/acme/*"), older),
		updated(team("acme", "acme-mobile", "github.com/acme/api"), older),
		updated(team("clientco", "clientco-contractors", "github.com/shared/api", "github.com/clientco/*"), newer),
		personal("github.com/shared/*"))
	got := Overlaps(*s)
	if len(got) != 1 || got[0].Entry != "github.com/shared/api" || got[0].Winner.TeamID != "clientco-contractors" || got[0].Other.TeamID != "acme-platform" {
		t.Fatalf("Overlaps = %+v", got)
	}
	expect(t, s, repo(got[0].Entry), Send, got[0].Winner.TeamID)
}

// With three organizations on one repository, only the newest policy's wins are reported.
func TestOverlapsNameOnlyTheRealWinner(t *testing.T) {
	t.Parallel()
	s := fixture(t,
		updated(team("a", "a-team", "github.com/shared/api"), now.Add(-time.Hour)),
		updated(team("b", "b-team", "github.com/shared/api"), now.Add(-48*time.Hour)),
		updated(team("c", "c-team", "github.com/shared/api"), now.Add(-72*time.Hour)), personal())
	for _, o := range Overlaps(*s) {
		if o.Winner.TeamID != "a-team" {
			t.Errorf("Overlaps reports %s winning %s, but a-team collects it", o.Winner.TeamID, o.Entry)
		}
	}
	if n := len(Overlaps(*s)); n != 2 {
		t.Fatalf("Overlaps = %d, want a-team over b-team and over c-team", n)
	}
}
