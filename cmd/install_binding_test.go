package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
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
	t.Setenv(shim.WrapperEnv, "")
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
	args := append([]string{"install", "--harness", "codex", "--no-hooks", "--no-path", "--no-doctor", "--no-browser"}, extra...)
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

func TestUnreachableBindingWording(t *testing.T) {
	cfg := &config.Config{Environment: config.EnvLocal, OrganizationID: orgA().ID, OrganizationName: "Acme"}
	// local and dev share an account service, so the environment is not the reason.
	got := unreachableBinding(&termaproject.File{Project: termaproject.Project{ID: testProjectID, Name: "Web", Environment: config.EnvDev}}, cfg)
	if want := "this repository is bound to Web, which is not a project in Acme"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
