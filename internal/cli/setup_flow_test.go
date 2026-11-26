package cli

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type recordingSpinner struct {
	mu     sync.Mutex
	events *[]string
	active bool
}

func (s *recordingSpinner) Start(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		*s.events = append(*s.events, "spinner:update:"+text)
	} else {
		s.active = true
		*s.events = append(*s.events, "spinner:start:"+text)
	}
}

func (s *recordingSpinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	*s.events = append(*s.events, "spinner:stop")
}

// policyAt is a per-repository policy body for repos, last updated at updated.
func policyAt(updated string, repos ...string) string {
	quoted := make([]string, len(repos))
	for i, r := range repos {
		quoted[i] = fmt.Sprintf("%q", r)
	}
	return fmt.Sprintf(`{"policy":{"version":"2.0","terma":{"per_repository":{"repositories":[%s]},"git_hooks":true,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":%q}`,
		strings.Join(quoted, ","), updated)
}

// setupMachine signs a fresh machine in to gateway, ready for `terma setup`.
func setupMachine(t *testing.T, gateway *fakeAuth) {
	t.Helper()
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
		t.Fatal(err)
	}
}

func enrolledTeams(t *testing.T) []string {
	t.Helper()
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load = %v, found=%v", err, found)
	}
	var ids []string
	for _, tm := range st.Teams {
		ids = append(ids, tm.TeamID)
	}
	return ids
}

// Setup signs in, fetches organizations and teams under one spinner, asks which teams to
// send to, then which agents, and enrolls only the teams left ticked.
func TestSetupOrderSpinnerAndTeamPicker(t *testing.T) {
	gateway := newFakeAuth(t)
	acmeWeb, acmeAPI, betaTeam := projectsIn(orgA().ID)[0].ID, projectsIn(orgA().ID)[1].ID, projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		acmeWeb:  policyAt("2026-11-01T00:00:00Z", "github.com/acme/web"),
		acmeAPI:  policyAt("2026-10-01T00:00:00Z", "github.com/shared/web"),
		betaTeam: policyAt("2026-11-01T00:00:00Z", "github.com/beta/core"),
	}
	setupMachine(t, gateway)

	var mu sync.Mutex
	var events []string
	prevSpin, prevAsk, prevForm, prevPick := testApp.newSpinner, testApp.canAsk, testApp.runForm, testApp.pickOne
	t.Cleanup(func() {
		testApp.newSpinner, testApp.canAsk, testApp.runForm, testApp.pickOne = prevSpin, prevAsk, prevForm, prevPick
	})
	testApp.newSpinner = func(io.Writer) progressSpinner { return &recordingSpinner{events: &events} }
	testApp.canAsk = func() bool { return true }
	testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) {
		t.Fatal("setup must not open a one-of picker")
		return 0, nil
	}
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		mu.Lock()
		events = append(events, "form:"+form.Title)
		mu.Unlock()
		items := slices.Clone(form.Items)
		for i := range items {
			// Untick Acme API in the team picker; choose Codex in the agent picker.
			items[i].Selected = items[i].Label == "Acme Web" || items[i].Label == "Beta Core" || strings.Contains(items[i].Label, "Codex")
		}
		return items, nil
	}

	out, err := runTerma(t, "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	want := []string{
		"spinner:start:Fetching organizations…",
		"spinner:update:Fetching teams…",
		"spinner:stop",
		"form:" + teamPickerTitle,
		"form:Which coding agents do you use?",
		// From the agent picker to the summary, one spinner names each step.
		"spinner:start:Applying team policies…",
		"spinner:update:Starting the relay…",
		"spinner:update:Writing agent hooks…",
		"spinner:update:Checking in…",
		"spinner:stop",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v\nwant   = %v", events, want)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{acmeWeb, betaTeam}) {
		t.Fatalf("enrolled %v, want Acme Web and Beta Core", got)
	}
	if strings.Contains(out, "Conflict") || strings.Contains(out, "Routing order") {
		t.Errorf("unexpected conflict for disjoint policies:\n%s", out)
	}

	// --verbose prints each step as it goes, so no spinner runs past the picker.
	events = nil
	if out, err := runTerma(t, "setup", "--verbose", "--harness", "codex"); err != nil {
		t.Fatalf("setup --verbose: %v\n%s", err, out)
	}
	if slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, "Applying team policies") }) {
		t.Fatalf("--verbose ran the progress spinner: %v", events)
	}
}

// A developer whose only organization is personal is asked nothing about teams.
func TestSetupPersonalOnlyAsksNoTeams(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgPersonal()}
	gateway.projects = map[string][]project{teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}}}
	gateway.policyBodies = map[string]string{teamsPersonalTeam: globalBody}
	setupMachine(t, gateway)

	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	var titles []string
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		titles = append(titles, form.Title)
		return form.Items, nil
	}
	out, err := runTerma(t, "setup")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if len(titles) != 1 || !strings.HasPrefix(titles[0], "Which coding agents") {
		t.Fatalf("forms = %v, want only the agent picker", titles)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{teamsPersonalTeam}) {
		t.Fatalf("enrolled %v, want personal alone", got)
	}
}

// Cancelling the team picker records nothing.
func TestSetupCancelAtTeamPickerCommitsNothing(t *testing.T) {
	gateway := newFakeAuth(t)
	setupMachine(t, gateway)
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) { return nil, prompt.ErrCancelled }

	out, err := runTerma(t, "setup")
	if err != nil {
		t.Fatalf("cancelled setup returned error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Cancelled. Nothing was recorded.") {
		t.Fatalf("unexpected cancellation output:\n%s", out)
	}
	if _, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err != nil || found {
		t.Fatalf("routing state written after cancelling (found=%v, err=%v)", found, err)
	}
}

// Overlapping policies, in one organization or two, are reported, never asked about: the
// newer policy collects.
func TestSetupReportsPolicyConflicts(t *testing.T) {
	gateway := newFakeAuth(t)
	acmeWeb, acmeAPI, betaTeam := projectsIn(orgA().ID)[0].ID, projectsIn(orgA().ID)[1].ID, projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		acmeWeb:  policyAt("2026-11-01T00:00:00Z", "github.com/shared/*"),
		acmeAPI:  policyAt("2026-10-01T00:00:00Z", "github.com/shared/web"),
		betaTeam: policyAt("2026-11-10T00:00:00Z", "github.com/shared/api", "github.com/beta/core"),
	}
	setupMachine(t, gateway)

	out, err := runTerma(t, "setup", "--harness", "codex", "--yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{
		"! Conflict 'Acme Web', 'Acme API' and 'Beta Core' list the same repositories.",
		doctorStep,
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Routing order") {
		t.Errorf("setup printed the routing order:\n%s", out)
	}
	st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	d := routing.NewRouter(st).Route(routing.Location{Kind: routing.Repository, Repository: "github.com/shared/api"}, time.Now())
	if tm, _ := st.Team(d.EnrollmentID); d.Outcome != routing.Send || tm.TeamID != betaTeam {
		t.Fatalf("shared/api routed to %+v, want Beta Core", d)
	}
	d = routing.NewRouter(st).Route(routing.Location{Kind: routing.Repository, Repository: "github.com/shared/web"}, time.Now())
	if tm, _ := st.Team(d.EnrollmentID); d.Outcome != routing.Send || tm.TeamID != acmeWeb {
		t.Fatalf("shared/web routed to %+v, want Acme Web", d)
	}
}

// A re-run that cannot read one ticked team's policy keeps that team, and its summary
// still names it beside the caution.
func TestSetupRerunSummaryKeepsATeamWhosePolicyFailed(t *testing.T) {
	gateway := newFakeAuth(t)
	acmeWeb, betaTeam := projectsIn(orgA().ID)[0].ID, projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		acmeWeb:  policyAt("2026-11-01T00:00:00Z", "github.com/acme/web"),
		betaTeam: policyAt("2026-11-10T00:00:00Z", "github.com/beta/core"),
	}
	setupMachine(t, gateway)
	if out, err := runTerma(t, "setup", "--harness", "codex", "--yes"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	gateway.forbiddenTeams = map[string]bool{betaTeam: true}
	out, err := runTerma(t, "setup", "--harness", "codex", "--yes")
	if err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if !slices.Contains(enrolledTeams(t), betaTeam) {
		t.Fatalf("re-run dropped Beta Core: %v", enrolledTeams(t))
	}
	if got := flat(out); !strings.Contains(got, "Beta Core: policy could not be read") || !regexp.MustCompile(`Sending to [^!]*Beta Labs › Beta Core`).MatchString(got) {
		t.Fatalf("re-run summary leaves out Beta Core:\n%s", out)
	}
}

// A personal organization with two teams gets one ticked, as routing takes one personal team.
func TestSetupTicksOnePersonalTeam(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgA(), orgPersonal()}
	gateway.projects = map[string][]project{
		orgA().ID: projectsIn(orgA().ID),
		teamsPersonalOrg: {
			{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg},
			{ID: "pppppppp-0000-4000-8000-000000000002", Name: "Side", OrganizationID: teamsPersonalOrg},
		},
	}
	gateway.policyBodies = map[string]string{teamsPersonalTeam: globalBody}
	setupMachine(t, gateway)
	if out, err := runTerma(t, "setup", "--harness", "codex", "--yes"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	got := enrolledTeams(t)
	if !slices.Contains(got, teamsPersonalTeam) || slices.Contains(got, "pppppppp-0000-4000-8000-000000000002") {
		t.Fatalf("enrolled %v, want the first personal team alone", got)
	}

	// Ticking both in the picker is refused before anything is committed.
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		items := slices.Clone(form.Items)
		for i := range items {
			items[i].Selected = true
		}
		return items, nil
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err == nil || !strings.Contains(err.Error(), "more than one Personal team") {
		t.Fatalf("setup ticking two personal teams: %v\n%s", err, out)
	}
}

// A re-run keeps the developer's choices: --yes enrolls no organization team left unticked,
// and --team adds the team it names to those ticked.
func TestSetupRerunKeepsTeamChoices(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgA(), orgB(), orgPersonal()}
	gateway.projects = map[string][]project{
		orgA().ID:        projectsIn(orgA().ID),
		orgB().ID:        projectsIn(orgB().ID),
		teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
	}
	acmeWeb, betaTeam := projectsIn(orgA().ID)[0].ID, projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{teamsPersonalTeam: globalBody}
	setupMachine(t, gateway)
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		items := slices.Clone(form.Items)
		for i := range items {
			items[i].Selected = items[i].Label == "Personal"
		}
		return items, nil
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup unticking every organization team: %v\n%s", err, out)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{teamsPersonalTeam}) {
		t.Fatalf("enrolled %v, want personal alone", got)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex", "--yes"); err != nil {
		t.Fatalf("setup --yes: %v\n%s", err, out)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{teamsPersonalTeam}) {
		t.Fatalf("--yes enrolled %v, want personal alone still", got)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex", "--yes", "--team", betaTeam); err != nil {
		t.Fatalf("setup --team: %v\n%s", err, out)
	}
	if got := enrolledTeams(t); !slices.Contains(got, betaTeam) || slices.Contains(got, acmeWeb) {
		t.Fatalf("--team enrolled %v, want Beta Core added and Acme Web still unticked", got)
	}
}

// With organization teams, the picker offers Personal too, and a re-run keeps it unticked
// once the developer unticked it.
func TestSetupPickerOffersPersonalAndKeepsItUnticked(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgA(), orgPersonal()}
	gateway.projects = map[string][]project{
		orgA().ID:        projectsIn(orgA().ID)[:1],
		teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
	}
	acmeWeb := projectsIn(orgA().ID)[0].ID
	gateway.policyBodies = map[string]string{teamsPersonalTeam: globalBody}
	setupMachine(t, gateway)
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	var offered []prompt.Item
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		items := slices.Clone(form.Items)
		if form.Title == teamPickerTitle {
			offered = slices.Clone(form.Items)
			for i := range items {
				items[i].Selected = items[i].Label != "Personal"
			}
		}
		return items, nil
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !slices.ContainsFunc(offered, func(it prompt.Item) bool { return it.Label == "Personal" && it.Selected }) {
		t.Fatalf("first setup offered %+v, want Personal ticked among the teams", offered)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{acmeWeb}) {
		t.Fatalf("enrolled %v, want Acme Web alone", got)
	}
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		if form.Title == teamPickerTitle {
			offered = slices.Clone(form.Items)
		}
		return form.Items, nil
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if !slices.ContainsFunc(offered, func(it prompt.Item) bool { return it.Label == "Personal" && !it.Selected }) {
		t.Fatalf("re-run offered %+v, want Personal unticked", offered)
	}
	if got := enrolledTeams(t); !slices.Equal(got, []string{acmeWeb}) {
		t.Fatalf("re-run enrolled %v, want Acme Web alone", got)
	}
}

// Setup's summary names the personal organization's team "Personal", whatever the team is
// called, and lists no repositories.
func TestSetupSummaryNamesPersonalAndListsNoRepositories(t *testing.T) {
	team := func(org, id string, personal bool, pol config.Policy) routing.Team {
		return routing.Team{OrganizationID: org, TeamID: id, Personal: personal, Policy: pol}
	}
	repos := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/web"}, TeamID: "w", FetchedAt: time.Now()}
	global := config.Policy{Mode: config.ModeGlobal, TeamID: "p", FetchedAt: time.Now()}
	var b strings.Builder
	u := newSetupUI(&b, false)
	names := teamNames{Orgs: map[string]string{"a": "Acme", "p": "Someone's Personal"}, Teams: map[string]string{"w": "Acme Web", "p": "Default"}}
	u.teamsConfigured([]organization{{ID: "a", Name: "Acme"}, {ID: "p", Name: "Someone's Personal", Type: "personal"}},
		routing.State{Teams: []routing.Team{team("a", "w", false, repos), team("p", "p", true, global)}}, names)
	u.printLines()
	out := b.String()
	if !strings.Contains(flat(out), "Sending to Acme › Acme Web You › Personal") || strings.Contains(out, "Default") || strings.Contains(out, "Collects") || strings.Contains(out, "github.com") {
		t.Fatalf("summary:\n%s", out)
	}
}
