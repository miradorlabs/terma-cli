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
		acmeAPI:  policyAt("2026-11-01T00:00:00Z", "github.com/acme/api"),
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
		"form:Send telemetry from this machine to: (space toggles, enter confirms)",
		"form:Which coding agents do you use? (space toggles, enter confirms)",
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

// Overlapping policies are reported, never asked about: the newer policy collects.
func TestSetupReportsPolicyConflicts(t *testing.T) {
	gateway := newFakeAuth(t)
	acmeWeb, acmeAPI, betaTeam := projectsIn(orgA().ID)[0].ID, projectsIn(orgA().ID)[1].ID, projectsIn(orgB().ID)[0].ID
	gateway.policyBodies = map[string]string{
		acmeWeb:  policyAt("2026-11-01T00:00:00Z", "github.com/shared/*"),
		acmeAPI:  policyAt("2026-11-01T00:00:00Z", "github.com/acme/api"),
		betaTeam: policyAt("2026-11-10T00:00:00Z", "github.com/shared/api", "github.com/beta/core"),
	}
	setupMachine(t, gateway)

	out, err := runTerma(t, "setup", "--harness", "codex", "--yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "github.com/shared/api is listed by") || !strings.Contains(out, "Beta Core collects it, as its policy is newer") {
		t.Fatalf("conflict not reported:\n%s", out)
	}
	order := regexp.MustCompile(`Routing order\s+1\. [^\n]*Beta Core — policy updated [^\n]*\n\s+2\. [^\n]*Acme Web — policy updated [^\n]*\n\s+3\. [^\n]*Acme API — policy updated `)
	if !order.MatchString(out) || !strings.Contains(out, "policies can change at any time") {
		t.Fatalf("routing order or its note missing:\n%s", out)
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
		if strings.HasPrefix(form.Title, "Send telemetry") {
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
		if strings.HasPrefix(form.Title, "Send telemetry") {
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
