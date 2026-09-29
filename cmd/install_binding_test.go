package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// boundRepo is a git repository bound to project, the test's own home and agent
// configuration around it, and the fake auth host behind it. signedIn stores an Acme
// session, which is what makes install check the binding: it signs in to route an agent.
func boundRepo(t *testing.T, bound termaproject.Project, signedIn bool) *fakeAuth {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	gitRepoHere(t)
	if signedIn {
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
			t.Fatal(err)
		}
	}
	if err := termaproject.Save(".", &termaproject.File{Project: bound}); err != nil {
		t.Fatal(err)
	}
	return f
}

func routeCodex(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"install", "--harness", "codex", "--no-hooks", "--no-doctor", "--no-browser"}, extra...)
	return within(20*time.Second).combined(t, args...)
}

func bindingNow(t *testing.T) termaproject.Project {
	t.Helper()
	f, err := termaproject.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	return f.Project
}

// A repository bound on the dev deployment, installed by a developer signed in to
// production: the project does not exist there. install used to take the binding as it
// stood and fail at the first key it minted, with the account service's own words — "no
// such project in this organization — run `mirador project list`". It says what is wrong
// now, before minting anything or touching the binding.
func TestInstallRefusesABindingFromAnotherEnvironment(t *testing.T) {
	devProject := termaproject.Project{
		ID: "dddddddd-0000-4000-8000-000000000001", Name: "Terma Dev",
		OrganizationID: "88dd0f5c-c288-4384-9dc6-9713281ad1ed", Environment: config.EnvDev,
	}
	f := boundRepo(t, devProject, true)
	before, err := os.ReadFile(termaproject.Path("."))
	if err != nil {
		t.Fatal(err)
	}

	out, err := routeCodex(t)
	if err == nil {
		t.Fatalf("install used a project the signed-in account cannot see:\n%s", out)
	}
	for _, want := range []string{"Terma Dev", "not a project in Acme", "bound in the dev environment", "terma is using production", "terma install --project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should say %q: %v", want, err)
		}
	}
	if strings.Contains(out+err.Error(), "mirador") {
		t.Errorf("the error names another product's CLI: %v", err)
	}
	if f.keysMint.Load() != 0 {
		t.Fatalf("minted %d keys for a project the account cannot see", f.keysMint.Load())
	}
	after, _ := os.ReadFile(termaproject.Path("."))
	if string(after) != string(before) {
		t.Fatalf("a refused install rewrote the binding:\n%s", after)
	}
}

// Same environment, another organization: the developer may belong to it, so the error
// names the switch as well as the rebind.
func TestInstallRefusesABindingFromAnotherOrganization(t *testing.T) {
	beta := projectsIn(orgB().ID)[0]
	f := boundRepo(t, termaproject.Project{ID: beta.ID, Name: beta.Name, OrganizationID: orgB().ID}, true)

	out, err := routeCodex(t)
	if err == nil {
		t.Fatalf("install used another organization's project:\n%s", out)
	}
	for _, want := range []string{"Beta Core", "not a project in Acme", "terma org use " + orgB().ID, "terma install --project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should say %q: %v", want, err)
		}
	}
	if f.keysMint.Load() != 0 {
		t.Fatalf("minted %d keys for another organization's project", f.keysMint.Load())
	}
}

// --project is the way out the error names: it binds the repository to one of the
// developer's projects, recorded with the environment it was chosen in.
func TestInstallProjectRebindsAnUnreachableBinding(t *testing.T) {
	boundRepo(t, termaproject.Project{ID: "dddddddd-0000-4000-8000-000000000001", Name: "Terma Dev", Environment: config.EnvDev}, true)

	if out, err := routeCodex(t, "--project", "Acme Web", "--yes"); err != nil {
		t.Fatalf("install --project: %v\n%s", err, out)
	}
	want := projectsIn(orgA().ID)[0]
	if got := bindingNow(t); got.ID != want.ID || got.Name != want.Name || got.OrganizationID != orgA().ID || got.Environment != "" {
		t.Fatalf("binding = %+v, want %s in production", got, want.Name)
	}
}

// A binding the account can see is kept exactly as committed without a terminal to ask
// on — a colleague's install must not churn the file.
func TestInstallKeepsAReachableBinding(t *testing.T) {
	acme := projectsIn(orgA().ID)[1]
	bound := termaproject.Project{ID: acme.ID, Name: acme.Name, OrganizationID: orgA().ID}
	f := boundRepo(t, bound, true)

	if out, err := routeCodex(t, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := bindingNow(t); got != bound {
		t.Fatalf("binding = %+v, want it kept as %+v", got, bound)
	}
	if f.keysMint.Load() == 0 {
		t.Fatal("routing Codex minted no key")
	}
}

// Wiring hooks with no agent of one's own needs no credential, so nothing checks the
// binding — and the binding keeps the environment it was made in instead of taking this
// machine's.
func TestInstallWithoutACredentialKeepsTheBindingsEnvironment(t *testing.T) {
	bound := termaproject.Project{ID: testProjectID, Name: "Terma Dev", Environment: config.EnvDev}
	boundRepo(t, bound, false)

	out, err := within(20*time.Second).combined(t, "install", "--harness", "none", "--adapters", "claude", "--yes", "--no-doctor", "--no-browser")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := bindingNow(t); got != bound {
		t.Fatalf("binding = %+v, want it kept as %+v", got, bound)
	}
}

// An organization with one project gives install nothing to choose, so it binds that
// project without the picker — even with a person there to ask, on a first install and in
// place of a binding the account cannot see alike. With several it still asks: here, with
// no terminal to draw the picker on, that is the picker's own error.
func TestResolveBindingTakesTheOnlyProjectWithoutAsking(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	resolve := func(org organization, existing *termaproject.File) (binding, string, error) {
		t.Helper()
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, org)); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetErr(&stderr)
		b, err := resolveBinding(cmd, cfg, existing, "", true, true)
		return b, stderr.String(), err
	}

	beta := projectsIn(orgB().ID)[0]
	if b, _, err := resolve(orgB(), nil); err != nil || b.ID != beta.ID || b.Name != beta.Name {
		t.Fatalf("first install: binding %+v, err %v; want %s without asking", b, err, beta.Name)
	}
	acme := projectsIn(orgA().ID)[0]
	b, stderr, err := resolve(orgB(), &termaproject.File{Project: termaproject.Project{ID: acme.ID, Name: acme.Name, OrganizationID: orgA().ID}})
	if err != nil || b.ID != beta.ID {
		t.Fatalf("unreachable binding: %+v, err %v; want it replaced by %s", b, err, beta.Name)
	}
	if !strings.Contains(stderr, "not a project in") {
		t.Errorf("install should still say why it replaced the binding: %q", stderr)
	}
	if _, _, err := resolve(orgA(), nil); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("an organization with several projects should go to the picker: %v", err)
	}
}

func TestUnreachableBindingWording(t *testing.T) {
	cfg := &config.Config{Environment: config.EnvLocal, OrganizationID: orgA().ID, OrganizationName: "Acme"}
	// local and dev share an account service, so the environment is not the reason.
	got := unreachableBinding(&termaproject.File{Project: termaproject.Project{ID: testProjectID, Name: "Web", Environment: config.EnvDev}}, cfg)
	if want := "this repository is bound to Web, which is not a project in Acme"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// terma_version is the terma that last wrote the repository's committed files: an
// install that writes none leaves an older one alone, and one that rewrites them moves it.
func TestInstallStampsTheVersionOnlyWhenItWritesCommittedFiles(t *testing.T) {
	repo := installRepo(t)
	install := func() {
		t.Helper()
		if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
	}
	install()
	bound, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	bound.Install.Version = "v0.0.1"
	if err := termaproject.Save(repo, bound); err != nil {
		t.Fatal(err)
	}

	install()
	if got, _ := termaproject.Load(repo); got.Install.Version != "v0.0.1" {
		t.Fatalf("an install that wrote nothing moved terma_version to %q", got.Install.Version)
	}

	settings := filepath.Join(repo, ".claude", "settings.json")
	data, _ := os.ReadFile(settings)
	var doc map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["hooks"], "Stop")
	stale, _ := json.Marshal(doc)
	if err := os.WriteFile(settings, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	install()
	if got, _ := termaproject.Load(repo); got.Install.Version != Version {
		t.Fatalf("an install that rewrote the hooks left terma_version at %q, want %q", got.Install.Version, Version)
	}
}

// --prompts is the one switch for whether the developer's agents send what was said. It is
// the machine's choice now (`terma setup` records it), and install still takes it — as a
// change to that choice — and the answer sticks: a re-install without it keeps the last
// one instead of switching prompts back on, which is what re-running install used to do.
func TestInstallPromptsSwitchSticks(t *testing.T) {
	acme := projectsIn(orgA().ID)[0]
	boundRepo(t, termaproject.Project{ID: acme.ID, Name: acme.Name, OrganizationID: orgA().ID}, true)
	prompts := func() bool {
		t.Helper()
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		st, err := (harness.Codex{}).Status()
		if err != nil || !st.Connected {
			t.Fatalf("Codex's global configuration: %+v, %v", st, err)
		}
		if st.IncludePrompts == cfg.Telemetry.ExcludePrompts {
			t.Fatalf("Codex's file (prompts %v) disagrees with the machine's record (excluded %v)", st.IncludePrompts, cfg.Telemetry.ExcludePrompts)
		}
		return st.IncludePrompts
	}
	for _, step := range []struct {
		args []string
		want bool
		line string
	}{
		// a first install sends them, unasked, and says how to stop it
		{nil, true, "prompt text and model responses are sent — `terma setup --prompts off` stops them"},
		{[]string{"--prompts", "off"}, false, "prompt text and model responses are not sent — `terma setup --prompts on` sends them"},
		{nil, false, ""}, // kept, not re-defaulted
		{[]string{"--prompts", "on"}, true, ""},
		{[]string{"--exclude-prompts"}, false, ""}, // the older spelling still works
	} {
		out, err := routeCodex(t, step.args...)
		if err != nil {
			t.Fatalf("install %v: %v\n%s", step.args, err, out)
		}
		if got := prompts(); got != step.want {
			t.Fatalf("after install %v: prompts included = %v, want %v", step.args, got, step.want)
		}
		if !strings.Contains(out, step.line) {
			t.Fatalf("install %v should say %q:\n%s", step.args, step.line, out)
		}
	}
	if _, err := routeCodex(t, "--yes", "--prompts", "maybe"); err == nil || !strings.Contains(err.Error(), "want on or off") {
		t.Fatalf("--prompts maybe: %v", err)
	}
	if _, err := routeCodex(t, "--yes", "--prompts", "on", "--exclude-prompts"); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("--prompts on --exclude-prompts: %v", err)
	}
}

func TestYesAnswer(t *testing.T) {
	for _, c := range []struct {
		line string
		def  bool
		want bool
	}{
		{"\n", true, true}, {"\n", false, false},
		{"y\n", false, true}, {"YES\n", false, true},
		{"n\n", true, false}, {"nah\n", true, false}, // a typo is never a yes
	} {
		if got := yesAnswer(c.line, c.def); got != c.want {
			t.Errorf("yesAnswer(%q, %v) = %v, want %v", c.line, c.def, got, c.want)
		}
	}
}
