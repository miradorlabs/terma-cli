package cli

import (
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

// The form lists every need ticked, an exact-only team's owners unticked, and a conflict
// in red as a pick-one that starts on deciding later.
func TestBindingsFormTicksEveryNeedAndPutsConflictsInRed(t *testing.T) {
	acme := needTick("org-a", "Acme", "github.com/acme/api", "github.com/acme/web", "github.com/shared/x")
	beta := needTick("org-b", "Beta", "github.com/beta/*", "github.com/shared/x")
	needs := append(tickNeeds(routing.State{}, acme), tickNeeds(routing.State{}, beta)...)
	form, slots := bindingsForm(needs)

	labels := func(match func(prompt.Item) bool) []string {
		var out []string
		for _, it := range form.Items {
			if match(it) {
				out = append(out, it.Label)
			}
		}
		return out
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Check && it.Selected }); !slices.Equal(got,
		[]string{"github.com/acme/api", "github.com/acme/web", "github.com/beta"}) {
		t.Errorf("ticked by default: %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Kind == prompt.Check && !it.Selected }); !slices.Equal(got,
		[]string{"the whole owner github.com/acme"}) {
		t.Errorf("offered unticked: %v (github.com/shared is needed by Beta too, so never offered whole)", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Alert }); !slices.Equal(got,
		[]string{"bind github.com/shared/x to Acme", "bind github.com/shared/x to Beta", "github.com/shared/x: decide later"}) {
		t.Errorf("in red: %v", got)
	}
	if got := labels(func(it prompt.Item) bool { return it.Alert && it.Selected }); !slices.Equal(got, []string{"github.com/shared/x: decide later"}) {
		t.Errorf("a conflict starts on %v, want deciding later", got)
	}

	keys := func(ops []neededOp) []string {
		var out []string
		for _, o := range ops {
			out = append(out, o.op.key+"="+o.op.org)
		}
		return out
	}
	answer := slices.Clone(form.Items)
	if got := keys(formOps(needs, slots, answer)); !slices.Equal(got, []string{"github.com/acme/api=org-a", "github.com/acme/web=org-a", "github.com/beta=org-b"}) {
		t.Errorf("the defaults bind %v", got)
	}
	for i, it := range answer {
		switch it.Label {
		case "the whole owner github.com/acme", "bind github.com/shared/x to Beta":
			answer[i].Selected = true
		case "github.com/beta", "github.com/shared/x: decide later":
			answer[i].Selected = false
		}
	}
	ops := formOps(needs, slots, answer)
	if got := keys(ops); !slices.Equal(got, []string{"github.com/acme=org-a", "github.com/shared/x=org-b"}) {
		t.Errorf("the whole owner, Beta for the conflict and github.com/beta left: %v", got)
	}
	if ops[0].tick != acme || ops[1].tick != beta {
		t.Error("a decision is not made with the tick that needs it")
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
		all, pending bool
		want         []string
	}{
		"neither":    {false, false, nil},
		"--yes":      {true, false, []string{"github.com/acme"}},
		"--bind-all": {true, true, []string{"github.com/old/repo", "github.com/acme"}},
	} {
		ops, err := decideNeeds(needs, false, tc.all, tc.pending)
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
