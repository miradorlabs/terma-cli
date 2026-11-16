package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// needTick is a tick for a team of org named orgName whose policy lists repos.
func needTick(org, orgName string, repos ...string) *tick {
	wildcards := slices.ContainsFunc(repos, func(r string) bool { return strings.HasSuffix(r, "/*") })
	return &tick{orgName: orgName, team: routing.Team{OrganizationID: org, TeamID: org + "-team",
		Policy: config.Policy{Mode: config.ModeRepo, Repositories: repos, Wildcards: wildcards}}}
}

// A key two organizations need, as an owner or as a repository, is a conflict; an owner
// one needs with a repository under it another needs is not, and a key decided already is
// no need at all.
func TestConflictsAreKeysMoreThanOneOrganizationNeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b []string
		want []string
	}{
		"the same owner":               {[]string{"github.com/acme/*"}, []string{"github.com/acme/*"}, []string{"github.com/acme"}},
		"the same repository":          {[]string{"github.com/acme/app"}, []string{"github.com/acme/app"}, []string{"github.com/acme/app"}},
		"an owner and a repository":    {[]string{"github.com/acme/*"}, []string{"github.com/acme/app"}, nil},
		"different repositories":       {[]string{"github.com/acme/api"}, []string{"github.com/acme/web"}, nil},
		"one organization's two teams": {[]string{"github.com/acme/api"}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			needs := tickNeeds(routing.State{}, needTick("org-a", "Acme", tc.a...))
			if tc.b != nil {
				needs = append(needs, tickNeeds(routing.State{}, needTick("org-b", "Beta", tc.b...))...)
			} else {
				needs = append(needs, tickNeeds(routing.State{}, needTick("org-a", "Acme", tc.a...))...)
			}
			if got, _ := conflicts(needs); !slices.Equal(got, tc.want) {
				t.Errorf("conflicts = %v, want %v", got, tc.want)
			}
		})
	}
	st := routing.State{Bindings: map[string]routing.Binding{"github.com/acme": {Organization: "org-b"}}}
	tk := needTick("org-a", "Acme", "github.com/acme/*")
	if needs := tickNeeds(st, tk); len(needs) != 0 || len(tk.notes) != 1 {
		t.Errorf("an owner bound elsewhere: needs %+v, notes %v", needs, tk.notes)
	}
}

// The form lists every need ticked; an exact-only team's owner as a pick between its listed
// repositories (the default) and the whole owner, which `a` cannot tick; and a conflict in
// red as a pick-one that starts on deciding later.
func TestBindingsFormTicksEveryNeedAndPutsConflictsInRed(t *testing.T) {
	acme := needTick("org-a", "Acme", "github.com/acme/api", "github.com/acme/web", "github.com/shared/x")
	beta := needTick("org-b", "Beta", "github.com/beta/*", "github.com/shared/x")
	needs := append(tickNeeds(routing.State{}, acme), tickNeeds(routing.State{}, beta)...)
	form, slots := bindingsForm(needs, needs)

	labels := func(match func(prompt.Item) bool) []string {
		var out []string
		for _, it := range form.Items {
			if match(it) {
				out = append(out, it.Label)
			}
		}
		return out
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Check }); !slices.Equal(got,
		[]string{"github.com/acme/api", "github.com/acme/web", "github.com/beta"}) {
		t.Errorf("checkboxes: %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Check && !it.Selected }); len(got) != 0 {
		t.Errorf("unticked by default: %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Radio && !it.Alert }); !slices.Equal(got,
		[]string{"only the listed repositories under github.com/acme", "the whole owner github.com/acme"}) {
		t.Errorf("the owner's pick: %v (github.com/shared is needed by Beta too, so never offered whole)", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Radio && !it.Alert && it.Selected }); !slices.Equal(got,
		[]string{"only the listed repositories under github.com/acme"}) {
		t.Errorf("the owner's pick starts on %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Alert }); !slices.Equal(got,
		[]string{"bind github.com/shared/x to Acme", "bind github.com/shared/x to Beta", "github.com/shared/x: decide later"}) {
		t.Errorf("in red: %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Alert && it.Selected }); !slices.Equal(got, []string{"github.com/shared/x: decide later"}) {
		t.Errorf("a conflict starts on %v, want deciding later", got)
	}

	keys := func(ds []decision) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.key+"="+d.org)
		}
		return out
	}
	answer := slices.Clone(form.Items)
	if got := keys(formDecisions(needs, slots, answer)); !slices.Equal(got, []string{"github.com/acme/api=org-a", "github.com/acme/web=org-a", "github.com/beta=org-b"}) {
		t.Errorf("the defaults bind %v", got)
	}
	for i, it := range answer {
		switch it.Label {
		case "the whole owner github.com/acme", "bind github.com/shared/x to Beta":
			answer[i].Selected = true
		case "github.com/beta", "github.com/shared/x: decide later", "only the listed repositories under github.com/acme":
			answer[i].Selected = false
		}
	}
	decided := formDecisions(needs, slots, answer)
	if got := keys(decided); !slices.Equal(got, []string{"github.com/acme=org-a", "github.com/shared/x=org-b"}) {
		t.Errorf("the whole owner, Beta for the conflict and github.com/beta left: %v", got)
	}
	if ops := withHolders(needs, decided); !slices.Equal(ops[0].holders, []*tick{acme}) || !slices.Equal(ops[1].holders, []*tick{beta}) {
		t.Error("a decision is not held by the ticks that need it")
	}
}

// An owner's many listed repositories are one line, so a long policy fits a terminal.
func TestBindingsFormCollapsesAnOwnersManyRepositories(t *testing.T) {
	var repos []string
	for i := range collapseAt + 2 {
		repos = append(repos, fmt.Sprintf("github.com/acme/r%d", i))
	}
	needs := tickNeeds(routing.State{}, needTick("org-a", "Acme", repos...))
	form, slots := bindingsForm(needs, needs)
	if len(form.Items) != 3 || form.Items[0].Label != fmt.Sprintf("github.com/acme: its %d listed repositories", collapseAt+2) {
		t.Fatalf("items: %+v", form.Items)
	}
	if ds := formDecisions(needs, slots, form.Items); len(ds) != collapseAt+2 {
		t.Errorf("the collapsed line binds %d repositories, want %d", len(ds), collapseAt+2)
	}
}

// Without a terminal, --bind-all (and --yes) bind every need one organization has, and
// leave a conflict to decide; never binding anything is the default.
func TestDecideNeedsBindsAllButConflicts(t *testing.T) {
	acme := needTick("org-a", "Acme", "github.com/acme/*", "github.com/shared/x")
	beta := needTick("org-b", "Beta", "github.com/shared/x")
	before := bindingNeed{key: "github.com/old/repo", org: "org-a", orgName: "Acme", entry: "github.com/old/repo"}
	needs := append([]bindingNeed{before}, append(tickNeeds(routing.State{}, acme), tickNeeds(routing.State{}, beta)...)...)
	for name, tc := range map[string]struct {
		mode bindingMode
		want []string
	}{
		"neither":    {leavePending, nil},
		"--yes":      {bindTicked, []string{"github.com/acme"}},
		"--bind-all": {bindPending, []string{"github.com/old/repo", "github.com/acme"}},
	} {
		ops, _, err := decideNeeds(needs, tc.mode, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, o := range ops {
			got = append(got, o.op.key)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s binds %v, want %v", name, got, tc.want)
		}
	}
}

// Two organizations' teams that both list one repository: --bind-all binds everything else
// and leaves that repository to decide, said as a conflict in the list and the JSON.
func TestTeamsBindAllLeavesAConflictToDecide(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	f.policyBodies[acmeAPI] = policyBody("github.com/acme/api", "github.com/shared/x")
	f.policyBodies[betaCore] = policyBody("github.com/beta/core", "github.com/shared/x")
	v, out, err := teamsJSON(t, "--select", "Acme API", "--select", "Beta Core", "--bind-all")
	if err != nil {
		t.Fatalf("teams --bind-all: %v\n%s", err, out)
	}
	st := loadState(t)
	if st.Bindings["github.com/acme/api"].Organization != orgA().ID || st.Bindings["github.com/beta/core"].Organization != orgB().ID {
		t.Errorf("bindings = %+v", st.Bindings)
	}
	if _, ok := st.Bindings["github.com/shared/x"]; ok {
		t.Errorf("a conflict was decided for the developer: %+v", st.Bindings)
	}
	if len(v.Pending) != 1 || !v.Pending[0].Conflict || len(v.Pending[0].Organizations) != 2 ||
		v.Pending[0].Organizations[0].Name == "" || v.Pending[0].Organizations[1].Name == "" {
		t.Fatalf("pending = %+v", v.Pending)
	}
	text, _ := runTerma(t, "teams")
	for _, want := range []string{"✗ github.com/shared/x: needed by more than one organization", "pick one: `terma teams --bind github.com/shared/x="} {
		if !strings.Contains(text, want) {
			t.Errorf("the list does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "`terma teams --bind-all`") {
		t.Errorf("nothing but the conflict is left, yet --bind-all is offered:\n%s", text)
	}
}

// What a run without --bind-all left to decide, a later --bind-all binds; the list offered
// it until then.
func TestTeamsBindAllBindsWhatWasLeftToDecide(t *testing.T) {
	migratedMachine(t, nil, orgA())
	if _, out, err := teamsJSON(t, "--select", "Acme API"); err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if text, _ := runTerma(t, "teams"); !strings.Contains(text, "`terma teams --bind-all`") {
		t.Errorf("the list does not offer --bind-all:\n%s", text)
	}
	v, out, err := teamsJSON(t, "--bind-all")
	if err != nil {
		t.Fatalf("teams --bind-all: %v\n%s", err, out)
	}
	if st := loadState(t); st.Bindings["github.com/acme/api"].Organization != orgA().ID || len(v.Pending) != 0 {
		t.Errorf("after --bind-all: bindings %+v, pending %+v", st.Bindings, v.Pending)
	}
}

// --yes leaves a conflict to decide too, instead of whichever team it ticked first taking it.
func TestTeamsYesLeavesAConflictToDecide(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	f.policyBodies[acmeAPI] = policyBody("github.com/shared/x")
	f.policyBodies[betaCore] = policyBody("github.com/shared/x")
	v, out, err := teamsJSON(t, "--select", "Acme API", "--select", "Beta Core", "--yes")
	if err != nil {
		t.Fatalf("teams --yes: %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Bindings) != 0 || len(v.Pending) != 1 || !v.Pending[0].Conflict {
		t.Errorf("bindings %+v, pending %+v", st.Bindings, v.Pending)
	}
}

// wildcardBody is a 2.0 policy response listing entries, wildcards among them.
func wildcardBody(entries ...string) string {
	return strings.Replace(policyBody(entries...), `"version":"1.1"`, `"version":"2.0"`, 1)
}

// onTerminal makes `terma teams` ask, answering each checklist with answer.
func onTerminal(t *testing.T, answer func(*prompt.Form) ([]prompt.Item, error)) {
	t.Helper()
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	testApp.canAsk, testApp.runForm = func() bool { return true }, answer
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
}

// enter answers a checklist as drawn, as Enter does.
func enter(f *prompt.Form) ([]prompt.Item, error) { return slices.Clone(f.Items), nil }

// An owner one organization needs and a repository under it another needs are both bound,
// the repository as an exception to the owner, whichever team is ticked first and whether
// the owner's team was ticked before.
func TestTeamsBindsAnOwnerAndARepositoryUnderItToTwoOrganizations(t *testing.T) {
	for name, runs := range map[string][][]string{
		"owner first":      {{"--select", "Acme API", "--select", "Beta Core", "--bind-all"}},
		"repository first": {{"--select", "Beta Core", "--select", "Acme API", "--bind-all"}},
		"with --yes":       {{"--select", "Acme API", "--select", "Beta Core", "--yes"}},
		"owner earlier":    {{"--select", "Acme API"}, {"--select", "Beta Core", "--bind-all"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := migratedMachine(t, nil, orgA(), orgB())
			f.policyBodies[acmeAPI] = wildcardBody("github.com/acme/*")
			f.policyBodies[betaCore] = policyBody("github.com/acme/api")
			for _, args := range runs {
				if _, out, err := teamsJSON(t, args...); err != nil {
					t.Fatalf("teams %v: %v\n%s", args, err, out)
				}
			}
			st := loadState(t)
			if st.Bindings["github.com/acme"].Organization != orgA().ID || st.Bindings["github.com/acme/api"].Organization != orgB().ID {
				t.Errorf("bindings = %+v", st.Bindings)
			}
		})
	}
}

// A team unticked in the run needs nothing: --bind-all binds none of its keys, and a key it
// shared with a team ticked in the same run is that team's alone.
func TestTeamsBindAllSkipsATeamUntickedInTheSameRun(t *testing.T) {
	f := migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api", "github.com/shared/x"))
		return err
	}, orgA(), orgB())
	f.policyBodies[betaCore] = policyBody("github.com/shared/x")
	v, out, err := teamsJSON(t, "--deselect", "Acme API", "--select", "Beta Core", "--bind-all")
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	st := loadState(t)
	if _, ok := st.Bindings["github.com/acme/api"]; ok {
		t.Errorf("an unticked team's key was bound: %+v", st.Bindings)
	}
	if st.Bindings["github.com/shared/x"].Organization != orgB().ID || len(v.Pending) != 0 {
		t.Errorf("bindings %+v, pending %+v: the shared key is Beta's alone", st.Bindings, v.Pending)
	}
}

// On a terminal (never with --json), a flag's own decision is never asked again, and
// --bind-all binds without asking; ticking with --select asks about what it ticked, and
// Enter binds the defaults.
func TestTeamsOnATerminalAsksOnlyWhatTheRunLeftOpen(t *testing.T) {
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api")); err != nil {
			return err
		}
		return s.Bind("github.com/acme/api", orgA().ID, false)
	}, orgA(), orgB())
	onTerminal(t, func(f *prompt.Form) ([]prompt.Item, error) {
		t.Errorf("asked: %+v", f.Items)
		return enter(f)
	})
	if out, err := runTerma(t, "teams", "--unbind", "github.com/acme/api"); err != nil {
		t.Fatalf("teams --unbind: %v\n%s", err, out)
	}
	if _, ok := loadState(t).Bindings["github.com/acme/api"]; ok {
		t.Fatal("--unbind was undone")
	}
	if out, err := runTerma(t, "teams", "--bind-all"); err != nil {
		t.Fatalf("teams --bind-all: %v\n%s", err, out)
	}
	if loadState(t).Bindings["github.com/acme/api"].Organization != orgA().ID {
		t.Fatal("--bind-all on a terminal did not bind")
	}

	asked := 0
	onTerminal(t, func(f *prompt.Form) ([]prompt.Item, error) {
		asked++
		return enter(f)
	})
	if out, err := runTerma(t, "teams", "--select", "Beta Core"); err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if asked != 1 || loadState(t).Bindings["github.com/beta/core"].Organization != orgB().ID {
		t.Errorf("asked %d times, bindings %+v", asked, loadState(t).Bindings)
	}
}

// Doctor names a conflict once, with every organization that needs it, and sends the
// developer to pick rather than binding it to the first.
func TestDoctorNamesAConflictOnce(t *testing.T) {
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/shared/x")); err != nil {
			return err
		}
		_, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/shared/x"))
		return err
	}, orgA(), orgB())
	out, _ := runTerma(t, "doctor")
	if n := strings.Count(out, "github.com/shared/x is needed by more than one organization"); n != 1 {
		t.Errorf("the conflict is named %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "terma teams, to pick whose github.com/shared/x is") || strings.Contains(out, "--bind github.com/shared/x=") {
		t.Errorf("doctor's fix is not to pick:\n%s", out)
	}
}

// Binding an owner would settle a conflict under it for that organization: it waits, said,
// and the conflict stays to decide.
func TestTeamsBindAllHoldsAnOwnerThatWouldSettleAConflict(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	f.policyBodies[acmeWeb] = wildcardBody("github.com/acme/*")
	f.policyBodies[acmeAPI] = policyBody("github.com/acme/api")
	f.policyBodies[betaCore] = policyBody("github.com/acme/api")
	_, out, err := teamsJSON(t, "--select", "Acme Web", "--select", "Acme API", "--select", "Beta Core", "--bind-all")
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	st := loadState(t)
	if len(st.Bindings) != 0 {
		t.Errorf("bindings = %+v: the owner would settle github.com/acme/api for Acme", st.Bindings)
	}
	if !strings.Contains(out, "github.com/acme is left to decide") {
		t.Errorf("not said:\n%s", out)
	}
}

// A key a team ticked before still needs conflicts with a team ticked now: --yes leaves it,
// and a terminal shows it in red.
func TestTeamsSelectSeesAConflictWithATeamTickedBefore(t *testing.T) {
	seed := func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/shared/x"))
		return err
	}
	f := migratedMachine(t, seed, orgA(), orgB())
	f.policyBodies[betaCore] = policyBody("github.com/shared/x")
	v, out, err := teamsJSON(t, "--select", "Beta Core", "--yes")
	if err != nil {
		t.Fatalf("teams --yes: %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Bindings) != 0 || len(v.Pending) != 1 || !v.Pending[0].Conflict {
		t.Fatalf("bindings %+v, pending %+v", st.Bindings, v.Pending)
	}

	f = migratedMachine(t, seed, orgA(), orgB())
	f.policyBodies[betaCore] = policyBody("github.com/shared/x")
	var red []string
	onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
		for _, it := range form.Items {
			if it.Alert {
				red = append(red, it.Label)
			}
		}
		return enter(form)
	})
	if out, err := runTerma(t, "teams", "--select", "Beta Core"); err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if len(red) != 3 || len(loadState(t).Bindings) != 0 {
		t.Errorf("red %v, bindings %+v", red, loadState(t).Bindings)
	}
}

// A failed tick takes back a decision only when no team that ticked, and none ticked before,
// still needs it.
func TestTeamsFailedTicksUndoOnlyWhatNoTeamStillNeeds(t *testing.T) {
	t.Run("both of an owner's teams fail", func(t *testing.T) {
		f := migratedMachine(t, nil, orgA())
		f.policyBodies[acmeWeb] = policyBody("github.com/acme/web")
		f.policyBodies[acmeAPI] = policyBody("github.com/acme/api")
		f.mintDownFor = map[string]bool{acmeWeb: true, acmeAPI: true}
		onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
			out := slices.Clone(form.Items)
			for i := range out {
				switch out[i].Label {
				case "the whole owner github.com/acme":
					out[i].Selected = true
				case "only the listed repositories under github.com/acme":
					out[i].Selected = false
				}
			}
			return out, nil
		})
		_, _ = runTerma(t, "teams", "--select", "Acme Web", "--select", "Acme API")
		if st := loadState(t); len(st.Bindings) != 0 || len(st.Teams) != 0 {
			t.Errorf("after both failed: bindings %+v, teams %d", st.Bindings, len(st.Teams))
		}
	})
	t.Run("a team ticked before still needs it", func(t *testing.T) {
		f := migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
			_, err := s.Select(stateTeam(f, orgA().ID, acmeWeb, false, "github.com/acme/api"))
			return err
		}, orgA())
		f.policyBodies[acmeAPI] = policyBody("github.com/acme/api")
		f.mintDownFor = map[string]bool{acmeAPI: true}
		_, _, _ = teamsJSON(t, "--select", "Acme API", "--bind-all")
		if st := loadState(t); st.Bindings["github.com/acme/api"].Organization != orgA().ID {
			t.Errorf("the earlier team's binding was undone: %+v", st.Bindings)
		}
	})
	t.Run("the other team that needs it ticks", func(t *testing.T) {
		f := migratedMachine(t, nil, orgA())
		f.policyBodies[acmeWeb] = policyBody("github.com/acme/shared")
		f.policyBodies[acmeAPI] = policyBody("github.com/acme/shared")
		f.mintDownFor = map[string]bool{acmeWeb: true}
		_, _, _ = teamsJSON(t, "--select", "Acme Web", "--select", "Acme API", "--yes")
		if st := loadState(t); st.Bindings["github.com/acme/shared"].Organization != orgA().ID || len(st.Teams) != 1 {
			t.Errorf("bindings %+v, teams %d", st.Bindings, len(st.Teams))
		}
	})
}

// The `terma teams` checklist asks about every key left to decide, earlier ones too, after
// its team list, and Enter binds them.
func TestTeamsChecklistAsksAboutEveryKeyLeftToDecide(t *testing.T) {
	f := migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeWeb, false, "github.com/acme/web"))
		return err
	}, orgA(), orgB())
	f.policyBodies[betaCore] = policyBody("github.com/beta/core")
	var forms int
	onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
		forms++
		out := slices.Clone(form.Items)
		if forms == 1 {
			for i := range out {
				if out[i].Label == "Beta Core" {
					out[i].Selected = true
				}
			}
		}
		return out, nil
	})
	if out, err := runTerma(t, "teams"); err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	st := loadState(t)
	if forms != 2 || st.Bindings["github.com/acme/web"].Organization != orgA().ID || st.Bindings["github.com/beta/core"].Organization != orgB().ID {
		t.Errorf("forms %d, bindings %+v", forms, st.Bindings)
	}
}
