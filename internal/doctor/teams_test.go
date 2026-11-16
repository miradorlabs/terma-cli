package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
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

// doctor shows what Mixed runs dropped, how many each reason turned Mixed, and for each run
// active lately why and where: the checkout it crossed into, or the command whose reach is
// not known.
func TestTeamsCheckAndRowsShowWhatMixedRunsDropped(t *testing.T) {
	t.Parallel()
	r := routed(t)
	r.Mixed = []runs.MixedRun{
		{Session: "s-1", Why: runs.Why{Reason: runs.Crossed, Where: "github.com/acme/api"}, Recent: true},
		{Session: "s-2", Why: runs.Why{Reason: runs.UnknownReach, Where: `cd "$(cat p)" && cat …`}, Recent: true},
		{Session: "s-3", Why: runs.Why{Reason: runs.UnknownReach}, Recent: true},
		{Session: "s-4", Why: runs.Why{Reason: runs.LocationBound}, Recent: true},
		{Session: "s-5", Why: runs.Why{Reason: runs.Aside}, Recent: true},
		{Session: "s-6", Recent: true},
	}
	r.MixedDropped = 7
	c := TeamsCheck(r, time.Now())
	if c.Status != Pass || !strings.Contains(c.Detail, "6 session run(s) Mixed, sending no telemetry until they resume: 2 ran a shell command whose reach is not known, 1 ") ||
		!strings.Contains(c.Detail, "1 crossed organizations") || !strings.Contains(c.Detail, "dropped 7 record(s)") {
		t.Fatalf("check = %+v", c)
	}
	rows := TeamRows(r, time.Now())
	var text []string
	for _, row := range rows {
		text = append(text, row.Label+"|"+row.Value)
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"Teams|Acme Platform — active, key ter_srv_0123…: github.com/acme/*", "Bindings|github.com/acme → Acme", "Mixed|",
		"|session s-1 crossed organizations, into github.com/acme/api",
		`|session s-2 ran a shell command whose reach is not known: cd "$(cat p)" && cat …`,
		"|session s-3 ran a shell command whose reach is not known\n",
		"|session s-4 touched more places than the run ledger keeps",
		"|session s-5 crossed while the run ledger was busy",
		"|session s-6 with a run ledger that cannot be read"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rows lack %q:\n%s", want, joined)
		}
	}
}

// doctor names the sessions of Mixed runs active within the last day, and counts the
// older ones the run ledger keeps.
func TestTeamsRowsCountOlderMixedRuns(t *testing.T) {
	t.Parallel()
	r := routed(t)
	r.Mixed = []runs.MixedRun{{Session: "s-1", Why: runs.Why{Reason: runs.Crossed}, Recent: true}}
	for _, s := range []string{"s-2", "s-3", "s-4"} {
		r.Mixed = append(r.Mixed, runs.MixedRun{Session: s, Why: runs.Why{Reason: runs.Crossed}})
	}
	if c := TeamsCheck(r, time.Now()); !strings.Contains(c.Detail, "4 session run(s) Mixed") || !strings.Contains(c.Detail, "4 crossed organizations (3 idle for over a day)") {
		t.Fatalf("check = %+v", c)
	}
	var sessions []string
	for _, row := range TeamRows(r, time.Now()) {
		if v, ok := strings.CutPrefix(row.Value, "session "); ok {
			sessions = append(sessions, v)
		}
	}
	if len(sessions) != 1 || sessions[0] != "s-1 crossed organizations" {
		t.Fatalf("rows name sessions %v", sessions)
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
	blocked.MigrationBlocked = &MigrationBlock{Detail: "the agents' hooks run another terma, /usr/local/bin/terma", Fix: "remove it", Fail: true}
	if c := check(Run(t.Context(), blocked, Progress{}), KeyMigration); c.Status != Fail || !strings.Contains(c.Detail, "/usr/local/bin/terma") || c.Fix != "remove it" {
		t.Errorf("migration blocked = %+v", c)
	}
	// One a later relay start may get past by itself warns.
	blocked.MigrationBlocked = &MigrationBlock{Detail: "the organization type could not be asked", Fix: "terma teams"}
	if c := check(Run(t.Context(), blocked, Progress{}), KeyMigration); c.Status != Warn || !strings.Contains(c.Detail, "organization type") {
		t.Errorf("migration deferred = %+v", c)
	}
	if c := check(Run(t.Context(), env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }}), Progress{}), KeyTeams); c.Key != "missing" {
		t.Errorf("a machine not migrated got a teams check: %+v", c)
	}
}

// A migrated machine whose routing state the move to routing by teams has not committed
// sends nothing: the teams check fails saying so, and names no team as collecting.
func TestDoctorFailsAMoveToTeamsNotFinished(t *testing.T) {
	t.Parallel()
	r := routed(t)
	r.Unfinished = "copy the sign-ins: keychain locked"
	if c := TeamsCheck(r, time.Now()); c.Status != Fail || !strings.Contains(c.Detail, "has not finished") || !strings.Contains(c.Detail, "keychain locked") {
		t.Errorf("teams = %+v", c)
	}
	if rows := TeamRows(r, time.Now()); len(rows) != 1 || !strings.Contains(rows[0].Value, "nothing is sent") {
		t.Errorf("rows = %+v", rows)
	}
	if c, team := RoutedRepository(r, "", nil, time.Now()); c.Status != Fail || team != nil {
		t.Errorf("repository = %+v, %v", c, team)
	}
}

// What the relay held, then dropped, for each pending binding and blocked team, as its
// last stop counted it, is shown by what blocked it.
func TestDoctorShowsWhatTheRelayHeld(t *testing.T) {
	t.Parallel()
	r := routed(t)
	r.Blocked = map[string]int{"pending:github.com/torvalds/linux": 3, "team:" + r.State.Teams[0].EnrollmentID: 2, "team:ended": 1}
	var text []string
	for _, row := range TeamRows(r, time.Now()) {
		text = append(text, row.Label+"|"+row.Value)
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"3 record(s) for github.com/torvalds/linux, undecided", "2 record(s) for Acme Platform, which could not send", "1 record(s) for ended, no longer ticked", "Relay held|"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rows lack %q:\n%s", want, joined)
		}
	}
}
