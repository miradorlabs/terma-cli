//go:build unix

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// globalSandbox makes the hooks setup writes call a built terma (the test binary is none).
func globalSandbox(t *testing.T) (codexHome string) {
	t.Helper()
	bin := termaBinary(t) // before HOME moves, and Go's caches with it
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome = t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig")) // global mode writes it
	prev := testApp.hookExecutable
	testApp.hookExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { testApp.hookExecutable = prev })
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
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

// Setup wires user-level agent hooks in either mode and leaves git's own configuration
// alone; teardown removes all of it.
func TestSetupInstallsMachineHooksInEveryMode(t *testing.T) {
	codexHome := globalSandbox(t)
	// The developer's own global git config, which setup must not touch.
	gitOut(t, t.TempDir(), "config", "--global", "user.name", "Dev")

	// Repository mode, the package's stub.
	out, err := runTerma(t, "setup", "--harness", "claude,codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	// Setup approves its own machine-wide Codex hooks, so there is no trust step to take.
	if !strings.Contains(out, "approved Terma's") || strings.Contains(out, "`/hooks`") {
		t.Fatalf("setup did not approve Codex's hooks itself:\n%s", out)
	}
	codexAgent, ok := testApp.agents.Find[agents.UserHooksTrust]("codex")
	if !ok {
		t.Fatal("no Codex agent in the registry")
	}
	if present, trusted, err := codexAgent.UserHooksTrusted(); err != nil || !present || !trusted {
		t.Fatalf("Codex would not run the machine-wide hooks: present %v trusted %v, %v", present, trusted, err)
	}
	claude, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	codex, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	for name, data := range map[string][]byte{"claude": claude, "codex": codex} {
		if !strings.Contains(string(data), "hook --user") {
			t.Errorf("%s has no machine-wide hooks:\n%s", name, data)
		}
	}
	// Nothing in git's configuration: a repository gets terma's hooks only when an agent
	// session is claimed in it, in its own .git/hooks.
	if got := gitConfig(t, "core.hooksPath"); got != "" {
		t.Fatalf("setup set git's global core.hooksPath to %q", got)
	}

	t.Setenv("TERMA_POLICY_STUB", globalStub)
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("global setup: %v\n%s", err, out)
	}
	if again, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json")); string(again) != string(codex) {
		t.Fatal("a setup in global mode rewrote Codex's hooks")
	}

	// Teardown: everything goes, and git's global hooks path is the developer's.
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	claude, _ = os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	codex, _ = os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if strings.Contains(string(claude)+string(codex), "hook --user") {
		t.Fatalf("machine-wide hooks left after teardown:\n%s\n%s", claude, codex)
	}
	if cfg, _ := os.ReadFile(filepath.Join(codexHome, "config.toml")); strings.Contains(string(cfg), "hooks.json:") {
		t.Fatalf("approvals of the removed hooks left in Codex's config:\n%s", cfg)
	}
	if got := gitConfig(t, "user.name"); got != "Dev" {
		t.Fatalf("teardown changed git's global config: user.name = %q", got)
	}
}

// gitConfig reads one global git setting, "" when it is unset.
func gitConfig(t *testing.T, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--global", "--get", key)
	cmd.Dir = t.TempDir()
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// The first claimed agent session in a collected repository installs terma's commit
// hooks there, chaining to the hook already in it; teardown leaves them be.
func TestAClaimedSessionInstallsTheRepositorysCommitHooks(t *testing.T) {
	globalSandbox(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "init", "-q")
	gitOut(t, root, "remote", "add", "origin", "git@github.com:acme/"+filepath.Base(root)+".git")
	admitHere(t, root, "team-hooks")
	pol, ok, err := config.ReadPolicy(testApp.stateDir, "team-hooks")
	if err != nil || !ok {
		t.Fatalf("no stored policy: %v", err)
	}
	pol.GitHooks = true
	selectPolicy(t, pol)
	hookruntest.RelayOn(t, testApp.stateDir)
	hooks := filepath.Join(root, ".git", "hooks")
	theirs := "#!/bin/sh\necho theirs\n"
	if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(theirs), 0o755); err != nil {
		t.Fatal(err)
	}

	c := testApp.newHookCommand()
	c.SetArgs([]string{"session-start"})
	c.SetIn(strings.NewReader(`{"session_id":"sess-install","cwd":"` + root + `","hook_event_name":"SessionStart","source":"startup"}`))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("terma hook session-start: %v", err)
	}
	for _, name := range []string{"prepare-commit-msg", "post-commit"} {
		data, err := os.ReadFile(filepath.Join(hooks, name))
		if err != nil || !strings.Contains(string(data), "hook "+name) {
			t.Fatalf("%s was not installed: %v\n%s", name, err, data)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(hooks, "prepare-commit-msg.pre-terma")); string(data) != theirs {
		t.Fatalf("the repository's own hook was not chained to:\n%s", data)
	}

	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(hooks, "prepare-commit-msg.pre-terma")); string(data) != theirs {
		t.Fatalf("teardown touched the repository's own hook:\n%s", data)
	}
	for _, name := range []string{"prepare-commit-msg", "post-commit"} {
		if data, err := os.ReadFile(filepath.Join(hooks, name)); err != nil || !strings.Contains(string(data), "hook "+name) {
			t.Errorf("teardown changed %s: %v", name, err)
		}
	}
}

// Where managed hooks are deployed, setup writes none of its own and removes earlier ones.
func TestSetupDefersToManagedHooks(t *testing.T) {
	codexHome := globalSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", globalStub)
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	// The organization deploys one agent's managed hooks afterwards.
	root := t.TempDir()
	prev := testApp.managedRoot
	testApp.managedRoot = root
	t.Cleanup(func() { testApp.managedRoot = prev })
	out := t.TempDir()
	if _, err := globalmode.WriteManaged(testApp.agents, out, "$HOME/.local/bin/terma"); err != nil {
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
}

// --managed-config writes the files an organization deploys and needs no sign-in.
func TestSetupWritesManagedConfig(t *testing.T) {
	useConfigDir(t, t.TempDir())
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

// nate puts back what setup changed outside the config directory it deletes: the agents'
// machine-wide hooks, and nothing of git's, which setup never touched.
func TestNateRestoresGlobalMode(t *testing.T) {
	globalSandbox(t)
	gitOut(t, t.TempDir(), "config", "--global", "user.name", "Dev")
	t.Setenv("TERMA_POLICY_STUB", globalStub)
	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	stubNateBinary(t, filepath.Join(t.TempDir(), "terma"))
	if out, err := runTerma(t, "nate", "--yes"); err != nil {
		t.Fatalf("nate: %v\n%s", err, out)
	}
	if got := gitConfig(t, "core.hooksPath"); got != "" {
		t.Errorf("core.hooksPath = %q, want none", got)
	}
	if data, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")); strings.Contains(string(data), "hook --user") {
		t.Errorf("machine-wide hooks survived nate:\n%s", data)
	}
}
