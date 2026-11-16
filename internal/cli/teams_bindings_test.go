package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
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
			if got, _ := conflicts(needs, indexNeeds(needs)); !slices.Equal(got, tc.want) {
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
	form, slots := bindingsForm(needs, needs, indexNeeds(needs))

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
			out = append(out, d.Key+"="+d.Organization)
		}
		return out
	}
	answer := slices.Clone(form.Items)
	if got, _ := formDecisions(needs, slots, answer); !slices.Equal(keys(got), []string{"github.com/acme/api=org-a", "github.com/acme/web=org-a", "github.com/beta=org-b"}) {
		t.Errorf("the defaults bind %v", keys(got))
	}
	for i, it := range answer {
		switch it.Label {
		case "the whole owner github.com/acme", "bind github.com/shared/x to Beta":
			answer[i].Selected = true
		case "github.com/beta", "github.com/shared/x: decide later", "only the listed repositories under github.com/acme":
			answer[i].Selected = false
		}
	}
	decided, replaced := formDecisions(needs, slots, answer)
	if got := keys(decided); !slices.Equal(got, []string{"github.com/acme=org-a", "github.com/shared/x=org-b"}) {
		t.Errorf("the whole owner, Beta for the conflict and github.com/beta left: %v", got)
	}
	if got := keys(replaced[decided[0]]); !slices.Equal(got, []string{"github.com/acme/api=org-a", "github.com/acme/web=org-a"}) {
		t.Errorf("the whole owner replaced %v", got)
	}
	if ops := withHolders(needs, decided, replaced, orgNames(needs)); !slices.Equal(ops[0].holders, []*tick{acme}) || !slices.Equal(ops[1].holders, []*tick{beta}) {
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
	form, slots := bindingsForm(needs, needs, indexNeeds(needs))
	if len(form.Items) != 3 || form.Items[0].Label != fmt.Sprintf("github.com/acme: its %d listed repositories", collapseAt+2) {
		t.Fatalf("items: %+v", form.Items)
	}
	if ds, _ := formDecisions(needs, slots, form.Items); len(ds) != collapseAt+2 {
		t.Errorf("the collapsed line binds %d repositories, want %d", len(ds), collapseAt+2)
	}
}

// Without a terminal, --bind-all (and --yes) bind every need one organization has, and
// leave a conflict to decide; never binding anything is the default.
func TestDecideNeedsBindsAllButConflicts(t *testing.T) {
	acme := needTick("org-a", "Acme", "github.com/acme/*", "github.com/shared/x")
	beta := needTick("org-b", "Beta", "github.com/shared/x")
	before := bindingNeed{Need: routing.Need{Key: "github.com/old/repo", Organization: "org-a"}, orgName: "Acme", entry: "github.com/old/repo"}
	needs := append([]bindingNeed{before}, append(tickNeeds(routing.State{}, acme), tickNeeds(routing.State{}, beta)...)...)
	for name, tc := range map[string]struct {
		mode bindingMode
		want []string
	}{
		"neither":    {leavePending, nil},
		"--yes":      {bindTicked, []string{"github.com/acme"}},
		"--bind-all": {bindPending, []string{"github.com/old/repo", "github.com/acme"}},
	} {
		ops, err := decideNeeds(needs, tc.mode, nil)
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

// orgC is a third organization, with a team ticked before the run.
const (
	orgC     = "cccccccc-3333-4333-8333-333333333333"
	orgCTeam = "cccccccc-0000-4000-8000-000000000001"
)

// pendingFor reports whether key is left to decide for org in st.
func pendingFor(st routing.State, key, org string) bool {
	return slices.ContainsFunc(doctor.Pending(st), func(p doctor.PendingBinding) bool { return p.Key == key && p.Organization == org })
}

// A repository an --unbind leaves undecided is never handed to its owner by what the same
// run binds: --bind-all and --yes leave the owner to decide and say why, the checklist
// shows the owner in red, and --bind on the owner asks.
func TestAnUnboundRepositoryIsNeverHandedToItsOwner(t *testing.T) {
	betaHasAPI := func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/acme/api")); err != nil {
			return err
		}
		return s.Bind("github.com/acme/api", orgB().ID, false)
	}
	acmeTicked := func(f *fakeAuth, s *routing.State) error {
		web := stateTeam(f, orgA().ID, acmeWeb, false, "github.com/acme/*")
		web.Policy.Wildcards = true
		if _, err := s.Select(web); err != nil {
			return err
		}
		return betaHasAPI(f, s)
	}
	leftToDecide := func(t *testing.T, out string) {
		t.Helper()
		st := loadState(t)
		if len(st.Bindings) != 0 || !pendingFor(st, "github.com/acme/api", orgB().ID) || !pendingFor(st, "github.com/acme", orgA().ID) {
			t.Errorf("bindings %+v, pending %+v: the owner would take github.com/acme/api from Beta", st.Bindings, doctor.Pending(st))
		}
		if out != "" && !strings.Contains(out, "github.com/acme is left to decide: binding it to Acme would also give it github.com/acme/api, which "+orgB().Name+" needs") {
			t.Errorf("not said:\n%s", out)
		}
	}
	t.Run("--bind-all", func(t *testing.T) {
		migratedMachine(t, acmeTicked, orgA(), orgB())
		out, err := runTerma(t, "teams", "--unbind", "github.com/acme/api", "--bind-all")
		if err != nil {
			t.Fatalf("teams: %v\n%s", err, out)
		}
		leftToDecide(t, out)
	})
	t.Run("--yes", func(t *testing.T) {
		f := migratedMachine(t, betaHasAPI, orgA(), orgB())
		f.policyBodies[acmeWeb] = wildcardBody("github.com/acme/*")
		out, err := runTerma(t, "teams", "--unbind", "github.com/acme/api", "--select", "Acme Web", "--yes")
		if err != nil {
			t.Fatalf("teams: %v\n%s", err, out)
		}
		leftToDecide(t, out)
	})
	t.Run("the checklist", func(t *testing.T) {
		f := migratedMachine(t, betaHasAPI, orgA(), orgB())
		f.policyBodies[acmeWeb] = wildcardBody("github.com/acme/*")
		var red []string
		onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
			for _, it := range form.Items {
				if it.Alert && !it.Selected {
					red = append(red, it.Label)
				}
			}
			return enter(form)
		})
		if out, err := runTerma(t, "teams", "--unbind", "github.com/acme/api", "--select", "Acme Web"); err != nil {
			t.Fatalf("teams: %v\n%s", err, out)
		}
		if !slices.Equal(red, []string{"github.com/acme"}) {
			t.Errorf("unticked in red: %v", red)
		}
		leftToDecide(t, "")
	})
	t.Run("--bind the owner", func(t *testing.T) {
		migratedMachine(t, acmeTicked, orgA(), orgB())
		out, err := runTerma(t, "teams", "--unbind", "github.com/acme/api", "--bind", "github.com/acme=Acme")
		if err == nil || !strings.Contains(err.Error(), "github.com/acme covers github.com/acme/api, which ") || !strings.Contains(err.Error(), "Pass --yes") {
			t.Errorf("--bind did not ask: %v\n%s", err, out)
		}
		if st := loadState(t); st.Bindings["github.com/acme/api"].Organization != orgB().ID || len(st.Bindings) != 1 {
			t.Errorf("bindings = %+v, want them as they were", st.Bindings)
		}
	})
}

// A whole owner picked in place of its organization's listed repositories stays bound when
// the exact-only team it was offered for fails to tick, while another team of the same
// organization that needed a repository under it ticks.
func TestUndoingAFailedTickKeepsAWholeOwnerATickedTeamNeeds(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	f.policyBodies[acmeWeb] = policyBody("github.com/acme/a")
	f.policyBodies[acmeAPI] = wildcardBody("github.com/other/*", "github.com/acme/c")
	f.mintDownFor = map[string]bool{acmeWeb: true}
	offered := false
	onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
		out := slices.Clone(form.Items)
		for i := range out {
			switch out[i].Label {
			case "the whole owner github.com/acme":
				out[i].Selected, offered = true, true
			case "only the listed repositories under github.com/acme":
				out[i].Selected = false
			}
		}
		return out, nil
	})
	out, err := runTerma(t, "teams", "--select", "Acme Web", "--select", "Acme API")
	if err == nil || !offered {
		t.Fatalf("Acme Web's key creation did not fail (%v), or the whole owner was not offered (%v):\n%s", err, offered, out)
	}
	st := loadState(t)
	if len(st.Teams) != 1 || st.Teams[0].TeamID != acmeAPI {
		t.Fatalf("ticked: %+v", st.Teams)
	}
	if st.Bindings["github.com/acme"].Organization != orgA().ID || pendingFor(st, "github.com/acme/c", orgA().ID) {
		t.Errorf("bindings %+v, pending %+v: Acme API's github.com/acme/c lost the owner that covered it", st.Bindings, doctor.Pending(st))
	}
}

// An owner bound beside a repository decision under it is unbound again when a failed tick
// undoes that decision and the owner would then take the repository from an organization
// ticked before: it is left to decide, and the failure says so.
func TestUndoingAFailedTickUnbindsAnOwnerThatWouldNowTakeARepository(t *testing.T) {
	f := migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgC, orgCTeam, false, "github.com/acme/api"))
		return err
	}, orgA(), orgB())
	f.policyBodies[acmeWeb] = wildcardBody("github.com/acme/*")
	f.policyBodies[betaCore] = policyBody("github.com/acme/api")
	f.mintDownFor = map[string]bool{betaCore: true}
	var picked []string
	onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
		out := slices.Clone(form.Items)
		for i := range out {
			switch out[i].Label {
			case "github.com/acme", "bind github.com/acme/api to " + orgB().Name:
				out[i].Selected = true
				picked = append(picked, out[i].Label)
			case "github.com/acme/api: decide later":
				out[i].Selected = false
			}
		}
		return out, nil
	})
	out, err := runTerma(t, "teams", "--select", "Acme Web", "--select", "Beta Core")
	if len(picked) != 2 || err == nil {
		t.Fatalf("picked %v, err %v:\n%s", picked, err, out)
	}
	if !strings.Contains(err.Error(), "github.com/acme is left to decide: binding it to Acme would also give it github.com/acme/api, which "+orgC+" needs") {
		t.Errorf("the failure does not say the owner was held: %v", err)
	}
	st := loadState(t)
	if len(st.Bindings) != 0 || !pendingFor(st, "github.com/acme/api", orgC) || !pendingFor(st, "github.com/acme", orgA().ID) {
		t.Errorf("bindings %+v, pending %+v: with Beta's pick undone, the owner takes github.com/acme/api", st.Bindings, doctor.Pending(st))
	}
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

// Enter binds an owner and a repository under it that another organization needs together,
// the repository as an exception to the owner: in a run that ticks both, in the checklist,
// and in a run that ticks the owner's team after the repository's, which shows the
// repository so it is decided in the same form.
func TestEnterBindsAnOwnerAndARepositoryUnderItTogether(t *testing.T) {
	both := func(t *testing.T) {
		t.Helper()
		if st := loadState(t); st.Bindings["github.com/acme"].Organization != orgA().ID || st.Bindings["github.com/acme/api"].Organization != orgB().ID {
			t.Errorf("bindings = %+v", st.Bindings)
		}
	}
	betaFirst := func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/acme/api"))
		return err
	}
	t.Run("one run ticks both", func(t *testing.T) {
		f := migratedMachine(t, nil, orgA(), orgB())
		f.policyBodies[acmeAPI] = wildcardBody("github.com/acme/*")
		f.policyBodies[betaCore] = policyBody("github.com/acme/api")
		onTerminal(t, enter)
		if out, err := runTerma(t, "teams", "--select", "Beta Core", "--select", "Acme API"); err != nil {
			t.Fatalf("teams --select: %v\n%s", err, out)
		}
		both(t)
	})
	t.Run("the checklist", func(t *testing.T) {
		f := migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
			acme := stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/*")
			acme.Policy.Wildcards = true
			if _, err := s.Select(acme); err != nil {
				return err
			}
			return betaFirst(f, s)
		}, orgA(), orgB())
		f.policyBodies[acmeAPI] = wildcardBody("github.com/acme/*")
		f.policyBodies[betaCore] = policyBody("github.com/acme/api")
		onTerminal(t, enter)
		if out, err := runTerma(t, "teams"); err != nil {
			t.Fatalf("teams: %v\n%s", err, out)
		}
		both(t)
	})
	t.Run("the repository's team ticked before", func(t *testing.T) {
		f := migratedMachine(t, betaFirst, orgA(), orgB())
		f.policyBodies[acmeAPI] = wildcardBody("github.com/acme/*")
		var drawn []prompt.Item
		onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
			drawn = form.Items
			return enter(form)
		})
		if out, err := runTerma(t, "teams", "--select", "Acme API"); err != nil {
			t.Fatalf("teams --select: %v\n%s", err, out)
		}
		if slices.ContainsFunc(drawn, func(it prompt.Item) bool { return it.Alert }) {
			t.Errorf("a line is red: %+v", drawn)
		}
		both(t)
	})
}

// A run with nothing to ask draws no form, though a team ticked before has a key left to
// decide that the run does not touch.
func TestTeamsSelectWithNothingToAskDrawsNoForm(t *testing.T) {
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api")); err != nil {
			return err
		}
		return s.Bind("github.com/beta/core", orgB().ID, false)
	}, orgA(), orgB())
	forms := 0
	onTerminal(t, func(form *prompt.Form) ([]prompt.Item, error) {
		forms++
		return enter(form)
	})
	if out, err := runTerma(t, "teams", "--select", "Beta Core"); err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if forms != 0 {
		t.Errorf("%d form(s) drawn", forms)
	}
}

// An owner whose binding would give away a repository under it that another organization
// needs is offered nowhere: doctor, the list and setup send the developer to that
// repository first, --bind on the owner asks, and binding both in one run asks nothing.
func TestAnOwnerThatWouldGiveARepositoryAwayWaitsForIt(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	f.policyBodies[acmeWeb] = wildcardBody("github.com/acme/*")
	f.policyBodies[acmeAPI] = policyBody("github.com/acme/api")
	f.policyBodies[betaCore] = policyBody("github.com/acme/api")
	if out, err := runTerma(t, "teams", "--select", "Acme Web", "--select", "Acme API", "--select", "Beta Core", "--bind-all"); err != nil {
		t.Fatalf("teams --bind-all: %v\n%s", err, out)
	}
	doc, _ := runTerma(t, "doctor")
	if strings.Contains(doc, "--bind github.com/acme=") || !strings.Contains(doc, "terma teams, to pick whose github.com/acme/api is") {
		t.Errorf("doctor's fix:\n%s", doc)
	}
	list, _ := runTerma(t, "teams")
	if strings.Contains(list, "--bind github.com/acme=") || strings.Contains(list, "--bind-all") || !strings.Contains(list, "decide github.com/acme/api first") {
		t.Errorf("the list:\n%s", list)
	}
	v, _, _ := teamsJSON(t)
	ui := newSetupUI(io.Discard, false)
	reportTeams(ui, v)
	if next := strings.Join(ui.next, "\n"); strings.Contains(next, "--bind-all") || !strings.Contains(next, "terma teams") {
		t.Errorf("setup's next step, with nothing --bind-all binds:\n%s", next)
	}
	if out, err := runTerma(t, "teams", "--bind", "github.com/acme=Acme"); err == nil || len(loadState(t).Bindings) != 0 {
		t.Errorf("bound without asking: %v, %+v\n%s", err, loadState(t).Bindings, out)
	}
	if out, err := runTerma(t, "teams", "--bind", "github.com/acme/api=Beta", "--bind", "github.com/acme=Acme"); err != nil {
		t.Fatalf("teams --bind both: %v\n%s", err, out)
	}
	if st := loadState(t); st.Bindings["github.com/acme"].Organization != orgA().ID || st.Bindings["github.com/acme/api"].Organization != orgB().ID {
		t.Errorf("bindings = %+v", st.Bindings)
	}
}

// Deciding scales with the needs, not their square: two organizations of 5,000
// repositories each, under 50 owners, bound all at once or drawn as a form; and checking
// the state written, with every owner bound to the other organization, so each takes one.
func BenchmarkDecideNeeds(b *testing.B) {
	var all []bindingNeed
	var written routing.State
	for _, org := range []string{"org-a", "org-b"} {
		repos := make([]string, 0, 5000)
		for i := range 5000 {
			repos = append(repos, fmt.Sprintf("github.com/%s-%d/r%d", org, i%50, i))
		}
		tk := needTick(org, org, repos...)
		all = append(all, tickNeeds(routing.State{}, tk)...)
		written.Teams = append(written.Teams, tk.team)
	}
	written.Bindings = map[string]routing.Binding{}
	keys := make([]string, 0, len(all))
	for _, n := range all {
		owner := routing.Owner(n.Key)
		if _, ok := written.Bindings[owner]; !ok {
			written.Bindings[owner] = routing.Binding{Organization: map[string]string{"org-a": "org-b", "org-b": "org-a"}[n.Organization]}
			keys = append(keys, owner)
		}
		keys = append(keys, n.Key)
	}
	b.Run("check", func(b *testing.B) {
		for b.Loop() {
			if taken := written.OwnersTaking(keys); len(taken) != 100 {
				b.Fatalf("%d owners take a repository, want 100", len(taken))
			}
		}
	})
	b.Run("bind-all", func(b *testing.B) {
		for b.Loop() {
			if _, err := decideNeeds(all, bindPending, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("form", func(b *testing.B) {
		for b.Loop() {
			if _, err := decideNeeds(all, askPending, enter); err != nil {
				b.Fatal(err)
			}
		}
	})
}
