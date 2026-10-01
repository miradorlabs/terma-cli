package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/install"
	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// boundRepo's signedIn stores a session, which is what makes install check the binding.
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

// A binding from another environment is named as the problem before anything is minted
// or the binding touched.
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
	for _, want := range []string{"Terma Dev", "not a team in Acme", "bound in the dev environment", "terma is using production", "terma install --team"} {
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

// Another organization's binding: the error names the switch as well as the rebind.
func TestInstallRefusesABindingFromAnotherOrganization(t *testing.T) {
	beta := projectsIn(orgB().ID)[0]
	f := boundRepo(t, termaproject.Project{ID: beta.ID, Name: beta.Name, OrganizationID: orgB().ID}, true)

	out, err := routeCodex(t)
	if err == nil {
		t.Fatalf("install used another organization's project:\n%s", out)
	}
	for _, want := range []string{"Beta Core", "not a team in Acme", "terma org use " + orgB().ID, "terma install --team"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should say %q: %v", want, err)
		}
	}
	if f.keysMint.Load() != 0 {
		t.Fatalf("minted %d keys for another organization's project", f.keysMint.Load())
	}
}

// --project rebinds, recorded with the environment it was chosen in.
func TestInstallProjectRebindsAnUnreachableBinding(t *testing.T) {
	boundRepo(t, termaproject.Project{ID: "dddddddd-0000-4000-8000-000000000001", Name: "Terma Dev", Environment: config.EnvDev}, true)

	if out, err := routeCodex(t, "--team", "Acme Web", "--yes"); err != nil {
		t.Fatalf("install --project: %v\n%s", err, out)
	}
	want := projectsIn(orgA().ID)[0]
	if got := bindingNow(t); got.ID != want.ID || got.Name != want.Name || got.OrganizationID != orgA().ID || got.Environment != "" {
		t.Fatalf("binding = %+v, want %s in production", got, want.Name)
	}
}

// Without a terminal a visible binding is kept exactly: a colleague's install must not churn it.
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

// An offline policy fixture without a credential keeps the binding and its environment unchecked.
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

// One project is bound without the picker; several still ask, which without a terminal
// is the picker's own error.
func TestResolveBindingTakesTheOnlyProjectWithoutAsking(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	resolve := func(org organization, existing *termaproject.File) (install.Binding, string, error) {
		t.Helper()
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, org)); err != nil {
			t.Fatal(err)
		}
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetErr(&stderr)
		b, err := testApp.resolveBinding(cmd, cfg, existing, "", true, true)
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
	if !strings.Contains(stderr, "not a team in") {
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
	if want := "this repository is bound to Web, which is not a team in Acme"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// terma_version moves only when install writes a committed file.
func TestInstallStampsTheVersionOnlyWhenItWritesCommittedFiles(t *testing.T) {
	repo := installRepo(t)
	install := func() {
		t.Helper()
		if out, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
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
	if got, _ := termaproject.Load(repo); got.Install.Version != testApp.version {
		t.Fatalf("an install that rewrote the hooks left terma_version at %q, want %q", got.Install.Version, testApp.version)
	}
}

// --prompts sticks: a re-install without it keeps the last choice.
func TestInstallPromptsSwitchSticks(t *testing.T) {
	acme := projectsIn(orgA().ID)[0]
	boundRepo(t, termaproject.Project{ID: acme.ID, Name: acme.Name, OrganizationID: orgA().ID}, true)
	prompts := func() bool {
		t.Helper()
		rec, ok, err := routing.LoadRecord(acme.ID)
		if err != nil || !ok {
			t.Fatalf("no routing record: ok=%v err=%v", ok, err)
		}
		return rec.IncludePrompts
	}
	for _, step := range []struct {
		args []string
		want bool
		line string
	}{
		// a first install sends them, unasked, and says how to stop it
		{nil, true, "prompt text and model responses are sent — `terma install --prompts off` stops them"},
		{[]string{"--prompts", "off"}, false, "prompt text and model responses are not sent — `terma install --prompts on` sends them"},
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

// --exclude-tool-content sticks like --prompts.
func TestInstallToolContentChoiceSticks(t *testing.T) {
	acme := projectsIn(orgA().ID)[0]
	boundRepo(t, termaproject.Project{ID: acme.ID, Name: acme.Name, OrganizationID: orgA().ID}, true)
	toolContent := func() bool {
		t.Helper()
		rec, ok, err := routing.LoadRecord(acme.ID)
		if err != nil || !ok {
			t.Fatalf("no routing record: ok=%v err=%v", ok, err)
		}
		return rec.IncludeToolContent
	}
	for _, step := range []struct {
		args []string
		want bool
	}{
		{nil, true}, // a first install sends it
		{[]string{"--exclude-tool-content"}, false},
		{nil, false}, // kept, not re-defaulted
		{[]string{"--exclude-tool-content=false"}, true},
	} {
		if out, err := routeCodex(t, step.args...); err != nil {
			t.Fatalf("install %v: %v\n%s", step.args, err, out)
		}
		if got := toolContent(); got != step.want {
			t.Fatalf("after install %v: tool content included = %v, want %v", step.args, got, step.want)
		}
	}
}

// An unbound repository takes the team chosen at setup without asking for one.
func TestInstallBindsTheTeamChosenAtSetup(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	gitRepoHere(t)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	chosen := projectsIn(orgA().ID)[1] // not the first, so nothing else picks it
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
		p.OrganizationID = orgA().ID
		p.Policy = &config.Policy{Mode: config.ModeRepo, TeamID: chosen.ID, OrganizationID: orgA().ID, AuthURL: os.Getenv("TERMA_AUTH_URL"), IncludePrompts: true, FetchedAt: time.Now()}
	}); err != nil {
		t.Fatal(err)
	}

	// No terminal: a prompt would fail the install.
	if out, err := routeCodex(t); err != nil {
		t.Fatalf("install asked for a team, or failed: %v\n%s", err, out)
	}
	if got := bindingNow(t); got.ID != chosen.ID {
		t.Fatalf("binding = %+v, want setup's team %s", got, chosen.Name)
	}
}
