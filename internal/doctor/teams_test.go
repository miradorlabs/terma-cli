package doctor

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

const (
	testAccount  = "0123456789abcdef"
	otherAccount = "fedcba9876543210"
	testAuthURL  = "https://auth.example.test"
	testOrgAcme  = "11111111-1111-4111-8111-111111111111"
	testOrgBeta  = "22222222-2222-4222-8222-222222222222"
	testOrgOwn   = "33333333-3333-4333-8333-333333333333"
	testTeamPlat = "platform"
	testTeamCore = "core"
	testTeamOwn  = "personal"
	testOrgGamma = "44444444-4444-4444-8444-444444444444"
	testTeamOps  = "ops"
)

func routedFixture(t *testing.T) Routing {
	t.Helper()
	now := time.Now().UTC()
	team := func(org, id string, personal bool, repos ...string) routing.Team {
		pol := config.Policy{
			Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: now,
			OrganizationID: org, AuthURL: testAuthURL, TeamID: id, Wildcards: true, GitHooks: true,
		}
		if personal {
			pol.Mode = config.ModeGlobal
		}
		return routing.Team{OrganizationID: org, AuthURL: testAuthURL, TeamID: id, Personal: personal, Policy: pol}
	}
	st := routing.State{Profile: config.DefaultProfile, Account: testAccount}
	for _, tm := range []routing.Team{
		team(testOrgAcme, testTeamPlat, false, "github.com/acme/*"),
		team(testOrgOwn, testTeamOwn, true),
	} {
		if _, err := st.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	return Routing{
		State:         st,
		ActiveAccount: testAccount,
		Teams:         map[string]string{testTeamPlat: "Platform", testTeamCore: "Core", testTeamOwn: "Personal"},
		Orgs:          map[string]string{testOrgAcme: "Acme", testOrgBeta: "Beta Labs", testOrgOwn: "Personal"},
	}
}

func TestTeamsCheckFailsClosedOnUnusableState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name   string
		mutate func(*Routing)
		want   string
		fix    string
	}{
		{
			name:   "unreadable",
			mutate: func(r *Routing) { r.Err = errors.New("corrupt json") },
			want:   "cannot be read",
			fix:    "terma setup",
		},
		{
			name:   "missing",
			mutate: func(r *Routing) { r.Missing = true },
			want:   "no routing state",
			fix:    "terma setup",
		},
		{
			name:   "signed_out",
			mutate: func(r *Routing) { r.ActiveAccount = "" },
			want:   "not signed in",
			fix:    "terma setup",
		},
		{
			name:   "account_unreadable",
			mutate: func(r *Routing) { r.ActiveAccount = sendas.AccountUnreadable },
			want:   "cannot be read or paired",
			fix:    "terma setup",
		},
		{
			name:   "wrong_account",
			mutate: func(r *Routing) { r.ActiveAccount = otherAccount },
			want:   "belongs to another account",
			fix:    "terma setup",
		},
		{
			name:   "wrong_account_server_key",
			mutate: func(r *Routing) { r.ActiveAccount = sendas.NoPersonLane; r.ServerKey = true },
			want:   "belongs to another account",
			fix:    "TERMA_API_KEY=<the team's server key> terma setup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := routedFixture(t)
			tc.mutate(&r)
			if c := TeamsCheck(r, now); c.Status != Fail || !strings.Contains(c.Detail, tc.want) || c.Fix != tc.fix {
				t.Errorf("TeamsCheck = %+v, want Fail containing %q with Fix %q", c, tc.want, tc.fix)
			}
			if c, team := RoutedRepository(r, "", nil, now); c.Status != Fail || team != nil || c.Fix != tc.fix {
				t.Errorf("RoutedRepository = %+v, %v", c, team)
			}
			if rows := TeamRows(r, now); len(rows) != 1 || !strings.Contains(rows[0].Value, tc.want) {
				t.Errorf("TeamRows = %+v", rows)
			}
		})
	}
}

func TestTeamsCheckEnrollmentHealthAndSingleSignInRemediation(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// Refused with OrgAccessLost fails and explains org access was lost.
	r := routedFixture(t)
	r.State.Teams[0].Refusals = 2
	r.State.Teams[0].LastError = routing.OrgAccessLost
	c := TeamsCheck(r, now)
	if c.Status != Fail || !strings.Contains(c.Detail, "refused (you no longer have access to its organization)") ||
		!strings.Contains(c.Fix, "terma teams --release Platform") {
		t.Fatalf("OrgAccessLost check = %+v", c)
	}

	// Suspended with Terminal fails and explains team is gone.
	r = routedFixture(t)
	r.State.Teams[0].Refusals = 1
	r.State.Teams[0].LastError = routing.Terminal
	c = TeamsCheck(r, now)
	if c.Status != Fail || !strings.Contains(c.Detail, "suspended (its organization says the team is gone)") {
		t.Fatalf("Terminal suspended check = %+v", c)
	}

	// SignInNeeded warns and remediates with a single `terma setup`, NEVER per-org `terma teams --org`.
	r = routedFixture(t)
	r.State.Teams[0].SignInNeeded = true
	r.State.Teams[1].SignInNeeded = true
	c = TeamsCheck(r, now)
	if c.Status != Warn || c.Fix != "terma setup" || strings.Contains(c.Detail, "--org") || strings.Contains(c.Fix, "--org") {
		t.Fatalf("SignInNeeded check = %+v, want Warn with Fix 'terma setup' and no per-org instructions", c)
	}

	// Expired policy warns.
	r = routedFixture(t)
	r.State.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour)
	c = TeamsCheck(r, now)
	if c.Status != Warn || !strings.Contains(c.Detail, "over a week") {
		t.Fatalf("Expired check = %+v", c)
	}

	// SignInNeeded always wins the recommended fix regardless of enrollment order
	// (even when an earlier enrollment is Expired or Refused).
	for _, tc := range []struct {
		name   string
		mutate func(*Routing)
		status Status
		fix    string
	}{
		{
			name: "expired_then_signin",
			mutate: func(r *Routing) {
				r.State.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour)
				r.State.Teams[1].SignInNeeded = true
			},
			status: Warn,
			fix:    "terma setup",
		},
		{
			name: "refused_then_signin",
			mutate: func(r *Routing) {
				r.State.Teams[0].Refusals = 2
				r.State.Teams[0].LastError = routing.OrgAccessLost
				r.State.Teams[1].SignInNeeded = true
			},
			status: Fail,
			fix:    "terma setup",
		},
		{
			name: "signin_then_refused",
			mutate: func(r *Routing) {
				r.State.Teams[0].SignInNeeded = true
				r.State.Teams[1].Refusals = 2
				r.State.Teams[1].LastError = routing.OrgAccessLost
			},
			status: Fail,
			fix:    "terma setup",
		},
		{
			name: "expired_then_refused",
			mutate: func(r *Routing) {
				r.State.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour)
				r.State.Teams[1].Refusals = 2
				r.State.Teams[1].LastError = routing.OrgAccessLost
			},
			status: Fail,
			fix:    "terma teams --release Personal (or wait for its policy to come back)",
		},
		{
			name: "shell_metachars_release",
			mutate: func(r *Routing) {
				r.Teams[r.State.Teams[0].TeamID] = "R&D"
				r.State.Teams[0].Refusals = 2
				r.State.Teams[0].LastError = routing.OrgAccessLost
			},
			status: Fail,
			fix:    "terma teams --release 'R&D' (or wait for its policy to come back)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := routedFixture(t)
			tc.mutate(&r)
			c := TeamsCheck(r, now)
			if c.Status != tc.status || c.Fix != tc.fix {
				t.Fatalf("TeamsCheck = %+v, want status=%v fix=%q", c, tc.status, tc.fix)
			}
		})
	}
}

// The paused sessions get a section of their own, counted by why, never named; what the
// relay dropped is counted under the teams.
func TestTeamsCheckAndSectionsShowPausedSessionsAndDroppedRecords(t *testing.T) {
	t.Parallel()
	now := time.Now()
	r := routedFixture(t)
	r.Mixed = []runs.MixedRun{
		{Session: "s-1", Why: runs.Why{Reason: runs.Crossed, Where: "github.com/acme/api"}, Recent: true},
		{Session: "s-2", Why: runs.Why{Reason: runs.Crossed, Where: "github.com/acme/web"}, Recent: true},
		{Session: "s-3", Why: runs.Why{Reason: runs.UnknownReach, Where: `cd "$(cat p)" && cat …`}, Recent: false},
	}
	r.MixedDropped = 7
	r.Blocked = map[string]int{"team:" + r.State.Teams[0].EnrollmentID: 2}

	c := TeamsCheck(r, now)
	if c.Status != Pass || !strings.Contains(c.Detail, "3 session run(s) Mixed") || !strings.Contains(c.Detail, "dropped 7 record(s)") {
		t.Fatalf("TeamsCheck = %+v", c)
	}
	got := render(r.teamSections(now), 80)
	for _, want := range []string{
		"  ! 2 records for Acme › Platform could not be sent\n",
		"Paused sessions\n" +
			"  ! 3 sessions are paused\n" +
			"       2 worked in another organization's repository\n" +
			"       1 ran a command terma could not trace\n" +
			"  ! 7 records from paused sessions dropped\n" +
			"    A session pauses when it may touch another organization's work. It sends\n" +
			"    nothing until it resumes.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sections lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s-1") || strings.Contains(got, "cat p") {
		t.Errorf("sections name a session or its command:\n%s", got)
	}
}

// collectsFixture is routedFixture plus an older Beta Labs Core whose list overlaps Acme
// Platform's, and a Gamma Ops that lost access to its organization.
func collectsFixture(t *testing.T, now time.Time) Routing {
	t.Helper()
	r := routedFixture(t)
	r.Teams[testTeamOps], r.Orgs[testOrgGamma] = "Ops", "Gamma"
	r.State.Teams[0].Policy.Repositories = []string{"github.com/acme/one", "github.com/shared/tool"}
	r.State.Teams[0].Policy.UpdatedAt = now
	team := func(org, id string, age time.Duration, repos ...string) routing.Team {
		return routing.Team{OrganizationID: org, AuthURL: testAuthURL, TeamID: id, Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: now, UpdatedAt: now.Add(-age), Wildcards: true,
			OrganizationID: org, AuthURL: testAuthURL, TeamID: id,
		}}
	}
	for _, tm := range []routing.Team{
		team(testOrgBeta, testTeamCore, time.Hour, "github.com/acme/*", "github.com/shared/tool", "github.com/beta/core"),
		team(testOrgGamma, testTeamOps, 0, "github.com/gamma/*", "github.com/beta/core"),
	} {
		if _, err := r.State.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	ops := &r.State.Teams[slices.IndexFunc(r.State.Teams, func(t routing.Team) bool { return t.TeamID == testTeamOps })]
	ops.Refusals, ops.LastError = 2, routing.OrgAccessLost
	return r
}

// render draws sections as doctor does, plain, wrapped to cols.
func render(ss []Section, cols int) string {
	var b strings.Builder
	(&Printer{W: &b, P: style.Plain(), Cols: cols}).Sections(ss)
	return b.String()
}

// The teams read in routing order, each with its state and what it collects beneath it:
// the newer policy wins an overlap and names the loser, an entry listed twice shows once,
// a refused team is skipped and collects nothing, and Personal comes last.
func TestTeamSectionsShowWhatEachTeamCollects(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 9, 30, 0, 0, time.Local)
	r := collectsFixture(t, now)
	want := `Where your data goes
  ✓ 1. Acme › Platform  updated 26 Nov 09:30
       github.com/acme/one
          Also listed by Core. This policy is newer.
       github.com/shared/tool
          Also listed by Core. This policy is newer.
  ✓ 2. Beta Labs › Core  updated 26 Nov 08:30
       github.com/acme/*
       github.com/beta/core
  ✓ 3. Personal  always last
       everything else
  ✗ Gamma › Ops  no access to this organization, skipped
    Teams are tried top to bottom. The newest policy wins a shared repository.
`
	if got := render(r.teamSections(now), 80); got != want {
		t.Errorf("sections =\n%s\nwant\n%s", got, want)
	}

	// A winner that cannot send says so; what it lists waits rather than passing on.
	r.State.Teams[0].SignInNeeded = true
	if got := render(r.teamSections(now), 80); !strings.Contains(got, "  ! 1. Acme › Platform  sign in again to send\n       github.com/acme/one\n") {
		t.Errorf("held winner:\n%s", got)
	}

	// Personal with its own list shows it; an entry an organization also lists stays there.
	r.State.Teams[0].SignInNeeded = false
	own := &r.State.Teams[len(r.State.Teams)-1]
	own.Policy.Mode, own.Policy.Repositories = config.ModeRepo, []string{"github.com/me/dotfiles", "github.com/acme/one"}
	got := render(r.teamSections(now), 80)
	for _, w := range []string{
		"       github.com/acme/one\n          Also listed by Core and Personal. This policy is newer.\n",
		"  ✓ 3. Personal  always last\n       github.com/me/dotfiles\n  ✗ Gamma",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("sections lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "everything else") || strings.Contains(got, "gamma") {
		t.Errorf("sections collect everything else or a refused team's list:\n%s", got)
	}

	// A team that lists nothing says so.
	r.State.Teams[0].Policy.Repositories = nil
	if got := render(r.teamSections(now), 80); !strings.Contains(got, "  ✓ 1. Acme › Platform  no repositories yet\n  ✓ 2.") {
		t.Errorf("empty list:\n%s", got)
	}
}

// TeamName says each team once: Personal alone, no organization repeated, no generic name.
func TestTeamName(t *testing.T) {
	t.Parallel()
	for _, tc := range [][3]string{
		{"Mirador Dev", "Engineering", "Mirador Dev › Engineering"},
		{"Terma Demo", "Demo", "Terma Demo"},
		{"Acme", "Default", "Acme"},
		{"Acme", "Acme Web", "Acme Web"},
		{"Beta Labs", "Beta Core", "Beta Core"},
		{"", "Core", "Core"},
	} {
		if got := TeamName(tc[0], tc[1]); got != tc[2] {
			t.Errorf("TeamName(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
	r := routedFixture(t)
	if got := r.label(r.State.Teams[1]); got != "Personal" {
		t.Errorf("personal label = %q", got)
	}
}

func TestRoutedRepositoryAndDoctorCombineMixedWithOutboxLosses(t *testing.T) {
	t.Parallel()
	_, gitDir := listed(t)
	now := time.Now()
	r := routedFixture(t)
	r.Mixed = []runs.MixedRun{{Session: "s-mix", Why: runs.Why{Reason: runs.Crossed, Where: "github.com/beta/core"}, Recent: true}}
	r.MixedDropped = 4

	c, team := RoutedRepository(r, gitDir, nil, now)
	if c.Status != Pass || team == nil || team.TeamID != testTeamPlat ||
		!strings.Contains(c.Detail, "github.com/acme/one goes to Acme › Platform (github.com/acme/*)") {
		t.Fatalf("RoutedRepository = %+v, %v", c, team)
	}

	// Doctor combines Mixed runs (KeyTeams + TeamRows) with #152 outbox/spool losses (KeyOutbox)
	// in both Run and Local without losing any #152 loss reason.
	e := env(t, Probes{
		Credential: signedIn,
		Keys:       keyed,
		Spool:      func() SpoolState { return SpoolState{Open: true} },
		Relay: func() Relay {
			return Relay{
				OutboxLosses: map[string]int{"outbox_full": 2, "signed_out": 3, "no_destination": 5},
				OutboxLossAt: now,
			}
		},
	})
	e.Config.Policy = config.Policy{}
	e.Routing = &r

	rep := Run(t.Context(), e, Progress{})
	if tc := check(rep, KeyTeams); tc.Status != Pass || !strings.Contains(tc.Detail, "1 session run(s) Mixed") || !strings.Contains(tc.Detail, "dropped 4 record(s)") {
		t.Errorf("KeyTeams = %+v", tc)
	}
	if pc := check(rep, KeyProject); pc.Status != Pass || !strings.Contains(pc.Detail, "goes to Acme › Platform") {
		t.Errorf("KeyProject = %+v", pc)
	}
	if hc := check(rep, KeyHooks); !strings.Contains(hc.Detail, "installed the first time an agent session is claimed here") {
		t.Errorf("KeyHooks = %+v", hc)
	}
	if oc := check(rep, KeyOutbox); oc.Status != Warn ||
		!strings.Contains(oc.Detail, "2 because the outbox was full") ||
		!strings.Contains(oc.Detail, "3 collected while signed out") ||
		!strings.Contains(oc.Detail, "5 with no routed destination") {
		t.Errorf("KeyOutbox = %+v", oc)
	}

	local, err := Local(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	var rowText []string
	for _, row := range local.Rows {
		rowText = append(rowText, row.Label+"|"+row.Value)
	}
	joined := strings.Join(rowText, "\n")
	for _, want := range []string{
		"Where your data goes|1. Acme › Platform (",
		"Paused sessions|1 session is paused",
		"Repository|github.com/acme/one goes to Acme › Platform",
		"Records lost|terma dropped records it could not send",
		"5 with no routed destination",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Local rows lack %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "outbox was full") {
		t.Errorf("Local rows dropped #152 outbox_full loss:\n%s", joined)
	}
}
