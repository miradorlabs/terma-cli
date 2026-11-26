package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Without --org, setup automatically discovers and enrolls teams across all organizations
// the signed-in user belongs to, binding repositories without prompting.
func TestSetupAutomaticallyEnrollsAllOrganizations(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.policyBodies = map[string]string{
		projectsIn(orgA().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		projectsIn(orgB().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	signInTo(t, gateway, orgA())

	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"Organizations Acme, Beta Labs", "Acme Web", "Beta Core"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output missing %q:\n%s", want, out)
		}
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load = %v, found=%v", err, found)
	}
	var ids []string
	for _, tm := range st.Teams {
		ids = append(ids, tm.TeamID)
	}
	if !slices.Contains(ids, projectsIn(orgA().ID)[0].ID) || !slices.Contains(ids, projectsIn(orgB().ID)[0].ID) {
		t.Fatalf("enrolled teams = %v, want both orgs' teams", ids)
	}
}

// --org narrows setup to the named organization without prompting.
func TestSetupOrgFlagScopesDiscovery(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.policyBodies = map[string]string{
		projectsIn(orgA().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/web"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		projectsIn(orgB().ID)[0].ID: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/beta/core"]},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	signInTo(t, gateway, orgA())

	out, err := runTerma(t, "setup", "--org", "beta", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup --org beta: %v\n%s", err, out)
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load = %v, found=%v", err, found)
	}
	if len(st.Teams) != 1 || st.Teams[0].OrganizationID != orgB().ID {
		t.Fatalf("teams = %+v, want only Beta Labs", st.Teams)
	}
	if org, _, _ := selected(t); org != orgB().ID {
		t.Fatalf("Beta Labs should be recorded on profile: %s", org)
	}
}

// signInTo stores u-1's sign-in with org chosen under it, as an earlier setup would have
// left them.
func signInTo(t *testing.T, f *fakeAuth, org organization) {
	t.Helper()
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization(org.ID, org.Name) }); err != nil {
		t.Fatal(err)
	}
}

// A setup that stops at the organization step leaves the credential where it was stored,
// though the sign-in before it saved the credential under the storage being tried.
func TestSetupStoppedAtTheOrganizationStepKeepsTheCredentialWhereItWas(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgsDown = true
	authSandbox(t, gateway)
	sandboxMachine(t)
	if err := saveSignIn(testApp.dir, storedSession(gateway)); err != nil {
		t.Fatal(err)
	}
	if auth.StoredInFile(testApp.dir, config.DefaultProfile) {
		t.Skip("no keychain to keep the credential in")
	}
	out, err := runTerma(t, "setup", "--harness", "codex", "--insecure-storage")
	if err == nil || !strings.Contains(err.Error(), "list your organizations") {
		t.Fatalf("setup should stop at the organization step: %v\n%s", err, out)
	}
	if config.InsecureStorage(testApp.dir) || auth.StoredInFile(testApp.dir, config.DefaultProfile) {
		t.Fatal("the credential stayed in plain text after setup stopped")
	}
}
