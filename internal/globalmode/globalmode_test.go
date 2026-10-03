package globalmode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

func sandbox(t *testing.T) (Machine, string) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	hooks := filepath.Join(t.TempDir(), "hooks.json")
	m := Machine{
		Agents:      agents.New(agentstest.UserHooked{Agent: agentstest.Agent{ID: "fake", Label: "fake-cli"}, Path: hooks}, agentstest.Agent{ID: "plain"}),
		Terma:       func() (string, error) { return "/opt/terma/bin/terma", nil },
		ManagedRoot: t.TempDir(),
	}
	return m, hooks
}

// A covered agent's machine-wide hooks are written, and removed again.
func TestMachineWideHooksComeAndGo(t *testing.T) {
	m, hooks := sandbox(t)
	changed, err := m.ApplyUserHooks([]string{"fake"}, true)
	if err != nil || len(changed) != 1 || changed[0] != hooks {
		t.Fatalf("ApplyUserHooks = %v, %v", changed, err)
	}
	data, _ := os.ReadFile(hooks)
	if !strings.Contains(string(data), "hook --user fake-stop") {
		t.Fatalf("hooks file:\n%s", data)
	}
	if _, err := m.ApplyUserHooks([]string{"fake"}, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(hooks); strings.Contains(string(data), "hook --user") {
		t.Fatalf("machine-wide hooks left:\n%s", data)
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

// A clone whose own hooks path outranks git's global one (husky's) is routed through
// terma's hooks at worktree scope, chaining to its own: a commit runs both, a hook manager
// writing its path again changes nothing, and Remove leaves the clone's own setting.
func TestAHuskyCloneIsRoutedThroughTermasHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	m, _ := sandbox(t)
	ctx := context.Background()
	marks := t.TempDir()
	terma := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(terma, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "terma")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Terma = func() (string, error) { return terma, nil }
	root := t.TempDir()
	git(t, "init", "-q", root)
	git(t, "-C", root, "config", "user.email", "dev@example.com")
	git(t, "-C", root, "config", "user.name", "Dev")
	husky := filepath.Join(root, ".husky", "_")
	if err := os.MkdirAll(husky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(husky, "prepare-commit-msg"), []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "husky")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", root, "config", "core.hooksPath", ".husky/_")
	gitDir := filepath.Join(root, ".git")

	if changed, err := m.WireClone(ctx, root, gitDir); err != nil || !changed {
		t.Fatalf("WireClone = %v, %v", changed, err)
	}
	if changed, err := m.WireClone(ctx, root, gitDir); err != nil || changed {
		t.Fatalf("a second WireClone = %v, %v", changed, err)
	}
	git(t, "-C", root, "config", "core.hooksPath", ".husky/_") // npm install runs husky again
	// The scripts are sh, which Git for Windows runs too; the fake terma here is not an .exe.
	if runtime.GOOS != "windows" {
		git(t, "-C", root, "commit", "-q", "--allow-empty", "-m", "x")
		for _, who := range []string{"terma", "husky"} {
			if _, err := os.Stat(filepath.Join(marks, who)); err != nil {
				t.Errorf("the commit did not run %s's prepare-commit-msg", who)
			}
		}
	}
	if !IsCloneHooksDir(git(t, "-C", root, "config", "--get", "core.hooksPath")) {
		t.Fatal("git does not see terma's clone hooks")
	}

	if err := m.Remove(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := git(t, "-C", root, "config", "--get", "core.hooksPath"); got != ".husky/_" {
		t.Fatalf("core.hooksPath after Remove = %q, want the clone's own", got)
	}
}

// A clone with no hooks path of its own is git's global hooks' to run.
func TestAPlainCloneIsLeftAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	m, _ := sandbox(t)
	root := t.TempDir()
	git(t, "init", "-q", root)
	if changed, err := m.WireClone(context.Background(), root, filepath.Join(root, ".git")); err != nil || changed {
		t.Fatalf("WireClone = %v, %v", changed, err)
	}
	if _, worktree := gitx.HooksPathFS(filepath.Join(root, ".git")); worktree != "" {
		t.Fatalf("worktree hooks path = %q", worktree)
	}
}
