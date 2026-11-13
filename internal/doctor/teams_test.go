package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const authURL = "https://auth.example"

func routedTeam(org, team string, personal bool, repos ...string) routing.Team {
	pol := config.Policy{Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: time.Now().UTC(),
		OrganizationID: org, AuthURL: authURL, TeamID: team, GitHooks: true}
	if personal {
		pol.Mode, pol.Repositories = config.ModeGlobal, nil
	}
	return routing.Team{OrganizationID: org, AuthURL: authURL, TeamID: team, Personal: personal, Policy: pol}
}

// routed is a migrated machine's routing: Acme's Platform listing github.com/acme/*, with
// its owner bound to Acme, and personal.
func routed(t *testing.T) Routing {
	t.Helper()
	var st routing.State
	for _, tm := range []routing.Team{routedTeam("acme", "platform", false, "github.com/acme/*"), routedTeam("me", "personal", true)} {
		if _, err := st.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	st.Teams[0].Policy.Wildcards = true
	if err := st.Bind("github.com/acme", "acme", false); err != nil {
		t.Fatal(err)
	}
	return Routing{State: st, Teams: map[string]string{"platform": "Platform", "personal": "Personal"},
		Orgs: map[string]string{"acme": "Acme", "me": "Personal"}, Key: func(string) string { return "ter_srv_0123…" }}
}

// doctor fails while the routing state is unreadable or missing, a team is suspended or
// refused, or a binding is undecided; it passes, naming each team's state, otherwise.
func TestTeamsCheckFailsOnWhatBlocks(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if c := TeamsCheck(routed(t), now); c.Status != Pass || c.Detail != "Acme Platform (active), Personal (active)" {
		t.Fatalf("all well = %+v", c)
	}
	if c := TeamsCheck(Routing{Err: errors.New("parse")}, now); c.Status != Fail || !strings.Contains(c.Detail, "cannot be read") || c.Fix != "terma setup" {
		t.Errorf("unreadable = %+v", c)
	}
	if c := TeamsCheck(Routing{Missing: true}, now); c.Status != Fail || c.Fix != "terma setup" {
		t.Errorf("missing = %+v", c)
	}
	for _, refusals := range []int{1, 2} {
		r := routed(t)
		r.State.Teams[0].Refusals, r.State.Teams[0].LastError = refusals, routing.Terminal
		if c := TeamsCheck(r, now); c.Status != Fail || !strings.Contains(c.Fix, "terma teams --release Platform") {
			t.Errorf("%d refusals = %+v", refusals, c)
		}
	}
	r := routed(t)
	if err := r.State.Unbind("github.com/acme"); err != nil {
		t.Fatal(err)
	}
	if c := TeamsCheck(r, now); c.Status != Fail || !strings.Contains(c.Detail, "github.com/acme is undecided") || !strings.Contains(c.Fix, "--bind github.com/acme=Acme") {
		t.Errorf("pending = %+v", c)
	}
	r = routed(t)
	r.State.Teams[0].SignInNeeded = true
	if c := TeamsCheck(r, now); c.Status != Warn || c.Fix != "terma teams --org Acme" {
		t.Errorf("sign-in needed = %+v", c)
	}
}

// doctor shows what Mixed runs dropped.
func TestTeamsCheckAndRowsShowWhatMixedRunsDropped(t *testing.T) {
	t.Parallel()
	r := routed(t)
	r.MixedSessions, r.MixedDropped = []string{"s-1"}, 7
	c := TeamsCheck(r, time.Now())
	if c.Status != Pass || !strings.Contains(c.Detail, "1 session run(s) crossed organizations") || !strings.Contains(c.Detail, "dropped 7 record(s)") {
		t.Fatalf("check = %+v", c)
	}
	rows := TeamRows(r, time.Now())
	var text []string
	for _, row := range rows {
		text = append(text, row.Label+"|"+row.Value)
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"Teams|Acme Platform — active, key ter_srv_0123…: github.com/acme/*", "Bindings|github.com/acme → Acme", "Mixed|", "|session s-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rows lack %q:\n%s", want, joined)
		}
	}
}

// The repository check routes this repository: where it goes, or why it is held.
func TestRoutedRepositoryRoutesThisRepository(t *testing.T) {
	t.Parallel()
	_, gitDir := listed(t)
	r := routed(t)
	c, team := RoutedRepository(r, gitDir, nil, time.Now())
	if c.Status != Pass || team == nil || team.TeamID != "platform" || !strings.Contains(c.Detail, "github.com/acme/one goes to Acme Platform (github.com/acme/*)") {
		t.Fatalf("bound = %+v, %v", c, team)
	}
	if err := r.State.Unbind("github.com/acme"); err != nil {
		t.Fatal(err)
	}
	if c, team := RoutedRepository(r, gitDir, nil, time.Now()); c.Status != Warn || team != nil || !strings.Contains(c.Fix, "--bind github.com/acme=") {
		t.Fatalf("pending = %+v", c)
	}
	if c, team := RoutedRepository(r, "", nil, time.Now()); c.Status != Pass || team == nil || !team.Personal {
		t.Fatalf("outside git = %+v", c)
	}
}

// On a migrated machine doctor runs the teams check in place of the profile's one
// policy, and judges the commit hooks by the team the repository goes to; two installs
// blocking the migration fail on a machine not yet migrated.
func TestDoctorRoutesByTeamsOnceMigrated(t *testing.T) {
	t.Parallel()
	e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }})
	e.Config.Policy = config.Policy{}
	r := routed(t)
	e.Routing = &r
	rep := Run(t.Context(), e, Progress{})
	if c := check(rep, KeyTeams); c.Status != Pass {
		t.Errorf("teams = %+v", c)
	}
	if c := check(rep, KeyProject); c.Status != Pass || !strings.Contains(c.Detail, "goes to Acme Platform") {
		t.Errorf("repository = %+v", c)
	}
	// Platform turns git_hooks on and collects this repository: the hooks come with its first session.
	if c := check(rep, KeyHooks); !strings.Contains(c.Detail, "installed the first time an agent session is claimed here") {
		t.Errorf("commit hooks = %+v", c)
	}

	blocked := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }})
	blocked.MigrationBlocked = "the agents' hooks run another terma, /usr/local/bin/terma"
	if c := check(Run(t.Context(), blocked, Progress{}), KeyMigration); c.Status != Fail || !strings.Contains(c.Detail, "/usr/local/bin/terma") {
		t.Errorf("migration blocked = %+v", c)
	}
	if c := check(Run(t.Context(), env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }}), Progress{}), KeyTeams); c.Key != "missing" {
		t.Errorf("a machine not migrated got a teams check: %+v", c)
	}
}
