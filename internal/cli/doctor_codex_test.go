package cli

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// An agent that gates hooks behind trust runs none of them until trusted, in silence, so
// doctor has to say so.
func TestDoctorReportsCodexHooksAwaitingTrust(t *testing.T) {
	installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "Codex hooks present") {
		t.Fatalf("doctor should see the hooks:\n%s", out)
	}
	if !strings.Contains(out, "has not been shown them") {
		t.Fatalf("doctor should say Codex has not been shown the hooks:\n%s", out)
	}
	if !strings.Contains(out, "Settings → Hooks → Review") || !strings.Contains(out, "/hooks in Codex CLI") {
		t.Fatalf("doctor should explain trust for both Desktop-only and CLI users:\n%s", out)
	}
	// A warning about one agent, not a verdict on the repository: the other agent's hooks run.
	if !strings.Contains(out, "ok    commit hooks installed") || !strings.Contains(out, "warn  agent hooks run") {
		t.Fatalf("the commit hooks pass; only the agent hooks warn:\n%s", out)
	}
	if !strings.Contains(out, "Setup needs attention:") || strings.Contains(out, "Predicted coverage") {
		t.Fatalf("doctor should report readiness without an invented percentage:\n%s", out)
	}
	status, _ := runTerma(t, "status")
	if !strings.Contains(status, "1 of 2 agents can run theirs") {
		t.Fatalf("status should name the agent hooks too:\n%s", status)
	}
}

// A colleague's committed hooks for an agent this developer does not use cost nothing.
func TestDoctorDoesNotChargeForAnAgentTheDeveloperDoesNotUse(t *testing.T) {
	installRepo(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{"claude"} }); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "ok    agent hooks run") || strings.Contains(out, "let every agent run its hooks") {
		t.Fatalf("an agent the developer does not use should cost nothing:\n%s", out)
	}
}

// A repository installed without the adapter is not nagged about its trust prompt.
func TestDoctorIgnoresCodexTrustWithoutTheAdapter(t *testing.T) {
	installRepo(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := runTerma(t, "install", "--harness", "none", "--team", testProjectID, "--adapters", "claude", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "Codex hooks") {
		t.Fatalf("doctor mentioned Codex hooks in a repository that has none:\n%s", out)
	}
}
