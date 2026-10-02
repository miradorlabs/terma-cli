package globalmode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func sandbox(t *testing.T) (Machine, string) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	hooks := filepath.Join(t.TempDir(), "hooks.json")
	relay := t.TempDir()
	m := Machine{
		Agents:      agents.New(agentstest.UserHooked{Agent: agentstest.Agent{ID: "fake", Label: "fake-cli"}, Path: hooks}, agentstest.Agent{ID: "plain"}),
		Terma:       func() (string, error) { return "/opt/terma/bin/terma", nil },
		ManagedRoot: t.TempDir(),
		RelayDir:    func() (string, error) { return relay, nil },
	}
	return m, hooks
}

// Global mode writes a covered agent's machine-wide hooks, and its committed hooks then
// step aside; leaving global mode removes both.
func TestMachineWideHooksComeAndGoWithGlobalMode(t *testing.T) {
	m, hooks := sandbox(t)
	global := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p"}
	if m.Yields(false, global, "fake-cli", "") {
		t.Fatal("a committed hook yielded before any machine-wide hook was written")
	}
	changed, err := m.ApplyUserHooks([]string{"fake"}, true)
	if err != nil || len(changed) != 1 || changed[0] != hooks {
		t.Fatalf("ApplyUserHooks = %v, %v", changed, err)
	}
	data, _ := os.ReadFile(hooks)
	if !strings.Contains(string(data), "hook --user fake-stop") {
		t.Fatalf("hooks file:\n%s", data)
	}
	if !m.Yields(false, global, "fake-cli", "") || m.Yields(false, config.DefaultPolicy(), "fake-cli", "") {
		t.Fatal("a committed hook did not step aside for the machine-wide one, or did outside global mode")
	}
	if !m.Yields(true, config.DefaultPolicy(), "fake-cli", "") || m.Yields(true, global, "fake-cli", "") {
		t.Fatal("a machine-wide hook acted outside global mode, or stood down inside it")
	}
	if _, err := m.ApplyUserHooks([]string{"fake"}, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(hooks); strings.Contains(string(data), "hook --user") {
		t.Fatalf("machine-wide hooks left:\n%s", data)
	}
	if m.Yields(false, global, "fake-cli", "") {
		t.Fatal("the coverage record outlived global mode")
	}
}

// git's global hooks path points at terma's hooks and comes back to the developer's own.
func TestGitHooksChainToTheDevelopersAndRestoreThem(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	m, _ := sandbox(t)
	ctx := context.Background()
	mine := t.TempDir()
	git(t, "config", "--global", "core.hooksPath", mine)
	if changed, err := m.ApplyGitHooks(ctx, true); err != nil || !changed {
		t.Fatalf("ApplyGitHooks(install) = %v, %v", changed, err)
	}
	ours := git(t, "config", "--global", "--get", "core.hooksPath")
	script, err := os.ReadFile(filepath.Join(ours, "prepare-commit-msg"))
	if err != nil || !strings.Contains(string(script), "hook prepare-commit-msg") || !strings.Contains(string(script), mine) {
		t.Fatalf("prepare-commit-msg does not run terma and chain to the developer's hooks:\n%s", script)
	}
	if changed, err := m.ApplyGitHooks(ctx, true); err != nil || changed {
		t.Fatalf("a second install changed git's configuration: %v, %v", changed, err)
	}
	if changed, err := m.ApplyGitHooks(ctx, false); err != nil || !changed {
		t.Fatalf("ApplyGitHooks(remove) = %v, %v", changed, err)
	}
	if got := git(t, "config", "--global", "--get", "core.hooksPath"); got != mine {
		t.Fatalf("core.hooksPath = %q, want %q back", got, mine)
	}
}

// Every script ends in success whatever terma does, and only two of them call it.
func TestGitHookScriptsNeverBlockGit(t *testing.T) {
	for _, hook := range gitHookNames {
		s := globalGitHookScript(hook, "/opt/terma", "")
		if !strings.HasSuffix(s, "exit 0\n") {
			t.Errorf("%s does not end in success:\n%s", hook, s)
		}
		if calls := strings.Contains(s, "/opt/terma"); calls != termaGitHooks[hook] {
			t.Errorf("%s calls terma = %v", hook, calls)
		}
		if termaGitHooks[hook] && !strings.Contains(s, "|| true") {
			t.Errorf("%s can fail on terma:\n%s", hook, s)
		}
	}
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
