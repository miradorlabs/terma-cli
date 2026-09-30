//go:build unix

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// globalSandbox signs a developer in on a private machine and makes the hooks that
// setup writes call a built terma (the test binary is none).
func globalSandbox(t *testing.T) (codexHome string) {
	t.Helper()
	bin := termaBinary(t) // before HOME moves, and Go's caches with it
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome = t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	prev := hookExecutable
	hookExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { hookExecutable = prev })
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	return codexHome
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const globalStub = `{"mode":"global","include_prompts":true,"include_tool_content":true,"default_project_id":"p-default"}`

// setup in global mode puts hooks for every session and every commit on the machine —
// the agents' user-level hooks and git's global hooks path, chaining the hooks git ran
// before — and a setup back in repo mode takes every one of them away again.
func TestSetupGlobalModeInstallsAndRemovesMachineHooks(t *testing.T) {
	codexHome := globalSandbox(t)
	// The developer's own global hooks directory, which git ran before terma's.
	mine := t.TempDir()
	marker := filepath.Join(t.TempDir(), "their-pre-commit-ran")
	if err := os.WriteFile(filepath.Join(mine, "pre-commit"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, t.TempDir(), "config", "--global", "core.hooksPath", mine)

	t.Setenv("TERMA_POLICY_STUB", globalStub)
	out, err := runTerma(t, "setup", "--harness", "claude,codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "every session on this machine") || !strings.Contains(out, "`/hooks`") {
		t.Fatalf("setup did not say global mode, or Codex's trust step:\n%s", out)
	}
	claude, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	codex, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	for name, data := range map[string][]byte{"claude": claude, "codex": codex} {
		if !strings.Contains(string(data), "hook --user") {
			t.Errorf("%s has no machine-wide hooks:\n%s", name, data)
		}
	}
	hooksDir := gitOut(t, t.TempDir(), "config", "--global", "--get", "core.hooksPath")
	if sameDir(hooksDir, mine) {
		t.Fatal("git's global hooks path was not pointed at terma's")
	}

	// An unbound repository: its commit runs terma's hooks and still the developer's.
	repo := t.TempDir()
	gitOut(t, repo, "init", "-q")
	gitOut(t, repo, "config", "user.email", "dev@example.com")
	gitOut(t, repo, "config", "user.name", "Dev")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "a.txt")
	gitOut(t, repo, "commit", "-q", "-m", "first")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the developer's own global pre-commit did not run under terma's hooks")
	}

	// A second setup changes nothing and keeps what terma replaced.
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("second setup: %v\n%s", err, out)
	}
	if again, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json")); string(again) != string(codex) {
		t.Fatal("a second global setup rewrote Codex's hooks")
	}

	// Back to repo mode: everything goes, and git's global hooks path is the developer's.
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo","include_prompts":true,"include_tool_content":true}`)
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("repo setup: %v\n%s", err, out)
	}
	claude, _ = os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	codex, _ = os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if strings.Contains(string(claude)+string(codex), "hook --user") {
		t.Fatalf("machine-wide hooks left after global mode ended:\n%s\n%s", claude, codex)
	}
	if got := gitOut(t, t.TempDir(), "config", "--global", "--get", "core.hooksPath"); !sameDir(got, mine) {
		t.Fatalf("git's global hooks path = %q, want the developer's %q back", got, mine)
	}
}

// In global mode a repository's committed hooks step aside for the agents the
// machine-wide hooks cover, and the machine-wide ones do nothing outside global mode.
func TestHookYields(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	global := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p"}
	repo := config.DefaultPolicy()
	if hookYields(false, global, "claude-code") {
		t.Fatal("a repository hook yielded with no machine-wide hooks recorded")
	}
	path, _ := userHooksRecordPath()
	if err := config.WriteJSON(path, userHooksRecord{Agents: []string{"claude", "codex"}}, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user  bool
		pol   config.Policy
		tool  string
		yield bool
	}{
		{false, global, "claude-code", true}, // the machine-wide hook handles it
		{false, global, "cursor", false},     // not covered: the repository's runs
		{true, global, "claude-code", false},
		{true, repo, "claude-code", true}, // leftover from global mode
		{false, repo, "claude-code", false},
	} {
		if got := hookYields(tc.user, tc.pol, tc.tool); got != tc.yield {
			t.Errorf("hookYields(user=%v, %s, %s) = %v", tc.user, tc.pol.Mode, tc.tool, got)
		}
	}
}

// Where the organization deployed terma's hooks as managed configuration, setup writes
// none of its own for that agent — and takes away any it wrote before the deployment —
// while a repository's committed hooks still step aside for it.
func TestSetupGlobalModeDefersToManagedHooks(t *testing.T) {
	codexHome := globalSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", globalStub)
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	// The organization deploys Codex's hooks afterwards.
	root := t.TempDir()
	prev := managedRoot
	managedRoot = root
	t.Cleanup(func() { managedRoot = prev })
	out := t.TempDir()
	if _, err := writeManagedConfig(out, "$HOME/.local/bin/terma"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(out, "codex-requirements.toml"))
	if err := os.MkdirAll(filepath.Join(root, "etc", "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "codex", "requirements.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	setupOut, err := runTerma(t, "setup", "--harness", "claude,codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, setupOut)
	}
	codex, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	claude, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	if strings.Contains(string(codex), "hook --user") {
		t.Fatalf("setup kept its own Codex hooks beside the managed ones:\n%s", codex)
	}
	if !strings.Contains(string(claude), "hook --user") {
		t.Fatal("Claude Code, with no managed hooks, lost its machine-wide ones")
	}
	if strings.Contains(setupOut, "`/hooks`") {
		t.Fatalf("setup asked to trust hooks the organization manages:\n%s", setupOut)
	}
	if !userHooksCover("codex") || !userHooksCover("claude-code") {
		t.Fatal("the agents' repository hooks would not step aside")
	}
}

// --managed-config writes the files an organization deploys and needs no sign-in.
func TestSetupWritesManagedConfig(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	out, err := runTerma(t, "setup", "--managed-config", dir)
	if err != nil {
		t.Fatalf("setup --managed-config: %v\n%s", err, out)
	}
	for _, f := range []string{"README.md", "claude-managed-settings.json", "codex-requirements.toml"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || (f != "README.md" && !strings.Contains(string(data), `"$HOME/.local/bin/terma" hook --user`) && !strings.Contains(string(data), `\"$HOME/.local/bin/terma\" hook --user`)) {
			t.Errorf("%s: %v\n%s", f, err, data)
		}
	}
}
