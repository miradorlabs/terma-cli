package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents/codex"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// An agent that gates hooks behind trust runs none of them until trusted, in silence, so
// doctor has to say so.
func TestDoctorReportsCodexHooksAwaitingTrust(t *testing.T) {
	installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
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
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
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

func TestDoctorAcceptsTrustedCodexHooks(t *testing.T) {
	repo := installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	// The agent writes a trust record for every entry once the developer trusts them.
	trustCodexEntries(t, repo, codexHome, func(codex.Entry) bool { return true })

	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "Codex hooks present and trusted") {
		t.Fatalf("doctor should report trusted hooks:\n%s", out)
	}
	if strings.Contains(out, "has not been shown them") {
		t.Fatalf("doctor still reports the hooks as unreviewed:\n%s", out)
	}
}

func TestDoctorRejectsChangedCodexHookAfterTrust(t *testing.T) {
	repo := installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	trustCodexEntries(t, repo, codexHome, func(codex.Entry) bool { return true })
	path := filepath.Join(codexHome, "config.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := codex.TermaEntries(repo)
	if err != nil {
		t.Fatal(err)
	}
	var oldHash string
	for _, entry := range entries {
		if entry.Event == "PostToolUse" {
			oldHash = entry.Hash
			break
		}
	}
	after := strings.Replace(string(before), oldHash, "sha256:stale", 1)
	if after == string(before) {
		t.Fatal("PostToolUse trust hash was not found")
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "PostToolUse") || !strings.Contains(out, "include any new or changed entries") {
		t.Fatalf("doctor accepted a changed, untrusted hook:\n%s", out)
	}
}

// A repository installed without the adapter is not nagged about its trust prompt.
func TestDoctorIgnoresCodexTrustWithoutTheAdapter(t *testing.T) {
	installRepo(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "Codex hooks") {
		t.Fatalf("doctor mentioned Codex hooks in a repository that has none:\n%s", out)
	}
}

func trustCodexEntries(t *testing.T, repo, codexHome string, keep func(codex.Entry) bool) {
	t.Helper()
	entries, err := codex.TermaEntries(repo)
	if err != nil || len(entries) == 0 {
		t.Fatalf("terma's Codex entries: %v (%d)", err, len(entries))
	}
	hooksPath := filepath.Join(repo, ".codex", "hooks.json")
	config := ""
	for _, e := range entries {
		if keep(e) {
			config += "[hooks.state.\"" + hooksPath + ":" + e.Key() + "\"]\n" + "trusted_hash = \"" + e.Hash + "\"\nenabled = true\n\n"
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Trust is per entry: entries added after the developer trusted the file are skipped in
// silence, and doctor names them.
func TestDoctorNamesTheCodexEntriesANewerTermaAdded(t *testing.T) {
	repo := installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	trustCodexEntries(t, repo, codexHome, func(e codex.Entry) bool { return !strings.HasPrefix(e.Event, "Subagent") })

	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "Codex hooks present and trusted") {
		t.Fatalf("doctor passed a file with untrusted entries:\n%s", out)
	}
	for _, want := range []string{"SubagentStart", "SubagentStop", "Settings → Hooks → Review", "include any new or changed entries"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not mention %q:\n%s", want, out)
		}
	}
}
