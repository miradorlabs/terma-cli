package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// personalBeta makes Beta Labs the developer's personal organization, its one team
// collecting everything with commit hooks on.
func personalBeta(f *fakeAuth) {
	f.orgs = []organization{orgA(), {ID: orgB().ID, Name: "Beta Labs", Type: "personal"}}
	f.policyBodies[betaCore] = globalBody
}

// On a migrated machine setup shows the team checklist in place of the pickers. A profile
// with no routing state starts one, saying earlier bindings are lost, and a new setup ticks
// personal, saying what personal collects and that it stamps commits.
func TestSetupOnAMigratedMachineTicksPersonalOnANewSetup(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	personalBeta(f)
	t.Setenv("CODEX_HOME", t.TempDir())
	path, _ := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex", "-y")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{
		"no routing state for this profile: bindings made on this machine before, if any, are lost",
		"Personal      collects everything not bound to an organization, and stamps commits in every repository an agent works in there",
		"Teams         Beta Labs Beta Core",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("setup lacks %q:\n%s", want, out)
		}
	}
	st := loadState(t)
	if len(st.Teams) != 1 || !st.Teams[0].Personal || st.Teams[0].TeamID != betaCore {
		t.Fatalf("teams = %+v", st.Teams)
	}
	if _, team, _ := selected(t); team != "" {
		t.Errorf("setup recorded team %q on a migrated profile", team)
	}
}

// Setup on a migrated profile keeps its teams, never personal unasked, and --team ticks one.
func TestSetupOnAMigratedMachineKeepsItsTeamsAndTicksTeam(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	personalBeta(f)
	t.Setenv("CODEX_HOME", t.TempDir())
	out, err := runTerma(t, "setup", "--harness", "codex", "-y", "--team", acmeAPI)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	st := loadState(t)
	if len(st.Teams) != 1 || st.Teams[0].TeamID != acmeAPI || st.Bindings["github.com/acme/api"].Organization != orgA().ID {
		t.Fatalf("state = %+v", st)
	}
	if !strings.Contains(out, "Teams         Acme API") || strings.Contains(out, "Personal      collects") {
		t.Errorf("setup:\n%s", out)
	}
}

// An unreadable routing state is set aside, never written over, and said.
func TestSetupSetsAnUnreadableRoutingStateAside(t *testing.T) {
	migratedMachine(t, nil, orgA())
	t.Setenv("CODEX_HOME", t.TempDir())
	path, _ := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex", "-y")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(path + ".unreadable"); err != nil || string(b) != "{" {
		t.Fatalf("the unreadable state was not kept: %q, %v", b, err)
	}
	if !strings.Contains(out, "could not be read") || !strings.Contains(out, "teams and bindings are lost") {
		t.Errorf("setup:\n%s", out)
	}
	loadState(t)
}

// On a machine not yet migrated setup runs as it always has: the profile's one team, and
// no routing state or v2 index is made.
func TestSetupOnAMachineNotMigratedKeepsItsOneTeam(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex", "-y")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if _, team, _ := selected(t); team == "" {
		t.Errorf("setup recorded no team:\n%s", out)
	}
	if _, ok, err := routing.Load(testApp.stateDir, config.DefaultProfile); ok || err != nil || config.Migrated(testApp.dir) {
		t.Errorf("setup on a machine not migrated made a routing state (%v, %v) or migrated it (%v)", ok, err, config.Migrated(testApp.dir))
	}
	if strings.Contains(out, "Teams ") {
		t.Errorf("setup showed the teams checklist before migration:\n%s", out)
	}
}

// A signed-out migrated machine's setup signs in once, every organization pre-checked on
// the page: each lands in credentials.v2.json, personal is active, and the new setup ticks
// personal's team.
func TestSetupSignsInToEveryOrganizationInOneApproval(t *testing.T) {
	f := migratedMachine(t, nil)
	personalBeta(f)
	t.Setenv("CODEX_HOME", t.TempDir())
	path, _ := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	b := newFakeBrowser(t, f)
	out, err := termaRun{stderr: b}.combined(t, "setup", "--harness", "codex", "-y", "--no-browser")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if links := b.approvalLinks(); len(links) != 1 || links[0].Get("codes") != "multiple" || links[0].Has("orgs") {
		t.Fatalf("approval links = %v, want one with every organization pre-checked", links)
	}
	if got := signedInOrgs(t); !slices.Equal(got, []string{orgA().ID, orgB().ID}) {
		t.Fatalf("signed in to %v, want both organizations", got)
	}
	if v2 := fileText(t, config.CredentialsV2Path(testApp.dir)); !strings.Contains(v2, orgA().ID) || !strings.Contains(v2, orgB().ID) {
		t.Fatalf("credentials.v2.json lacks a sign-in:\n%s", v2)
	}
	if _, err := os.Stat(config.CredentialsPath(testApp.dir)); err == nil {
		t.Fatal("a migrated machine's sign-in wrote credentials.json")
	}
	if cred, profile := activeOrg(t); cred != orgB().ID || profile != orgB().ID {
		t.Fatalf("active = %s, profile %s; want personal", cred, profile)
	}
	if st := loadState(t); len(st.Teams) != 1 || !st.Teams[0].Personal || st.Teams[0].TeamID != betaCore {
		t.Fatalf("teams = %+v", st.Teams)
	}
	if !strings.Contains(out, "Signed in to Beta Labs and Acme") || strings.Contains(out, "Not signed in") {
		t.Errorf("setup:\n%s", out)
	}
}

// fileText is the file at path, which must exist.
func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
