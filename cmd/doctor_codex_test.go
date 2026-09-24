package cmd

import (
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A repository can have perfect Codex wiring and still run none of it: Codex refuses a
// hook it has not been shown. That silence is the failure mode this adapter introduces,
// so doctor has to be the thing that breaks it.
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
	if !strings.Contains(out, "/hooks") {
		t.Fatalf("doctor should say how to fix it:\n%s", out)
	}
	// It is a warning about Codex, not a verdict on the repository: the commit hooks are
	// in and Claude Code's run, so commit stamping is worth half of what it could be —
	// not nothing, which is what one untrusted agent used to cost.
	if !strings.Contains(out, "ok    commit hooks installed") || !strings.Contains(out, "warn  agent hooks run") {
		t.Fatalf("the commit hooks pass; only the agent hooks warn:\n%s", out)
	}
	if !strings.Contains(out, "Setup needs attention:") || strings.Contains(out, "Predicted coverage") {
		t.Fatalf("doctor should report readiness without an invented percentage:\n%s", out)
	}
	// status tells the same story, with the same number.
	status, _ := runTerma(t, "status")
	if !strings.Contains(status, "1 of 2 agents can run theirs") {
		t.Fatalf("status should name the agent hooks too:\n%s", status)
	}
}

// An agent this developer does not use is not theirs to trust: a colleague's Codex hooks,
// committed in the repository, cost nothing here.
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
	// What Codex writes once the developer trusts the hooks from inside it: a record for
	// every entry terma installed.
	trustCodexEntries(t, repo, codexHome, func(hookmgr.CodexEntry) bool { return true })

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
	trustCodexEntries(t, repo, codexHome, func(hookmgr.CodexEntry) bool { return true })
	path := filepath.Join(codexHome, "config.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := hookmgr.CodexTermaEntries(repo)
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
	if !strings.Contains(out, "PostToolUse") || !strings.Contains(out, "review the new or changed entries") {
		t.Fatalf("doctor accepted a changed, untrusted hook:\n%s", out)
	}
}

// A repository installed without the Codex adapter is not missing anything, and must
// not be nagged about a trust prompt that does not apply to it.
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

// trustCodexEntries writes the trust records Codex keeps in the user's config, for the
// entries of the repository's hooks file that keep says the developer has trusted.
func trustCodexEntries(t *testing.T, repo, codexHome string, keep func(hookmgr.CodexEntry) bool) {
	t.Helper()
	entries, err := hookmgr.CodexTermaEntries(repo)
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

// Codex trusts a hook entry by entry. A developer who trusted terma's hooks before the
// two subagent entries existed has a file that counts as trusted and two hooks Codex
// skips without a word — which is every subagent in that repository going unrecorded.
func TestDoctorNamesTheCodexEntriesANewerTermaAdded(t *testing.T) {
	repo := installRepo(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	trustCodexEntries(t, repo, codexHome, func(e hookmgr.CodexEntry) bool { return !strings.HasPrefix(e.Event, "Subagent") })

	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "Codex hooks present and trusted") {
		t.Fatalf("doctor passed a file with untrusted entries:\n%s", out)
	}
	for _, want := range []string{"SubagentStart", "SubagentStop", "run /hooks to review the new or changed entries"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not mention %q:\n%s", want, out)
		}
	}
}
