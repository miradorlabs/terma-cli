package globalmode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
)

func sandbox(t *testing.T) (Machine, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	hooks := filepath.Join(t.TempDir(), "hooks.json")
	m := Machine{
		ConfigDir:   t.TempDir(),
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

// An agent's hooks file kept in a dotfiles repository behind a symlink is written through:
// the hooks land in the target and the link stays, and removing them keeps it too, even
// when they were all the file held (#118).
func TestMachineWideHooksWriteThroughASymlinkedFile(t *testing.T) {
	for _, seed := range []string{`{"theme":"dark"}`, `{}`} {
		t.Run(seed, func(t *testing.T) {
			m, hooks := sandbox(t)
			target := filepath.Join(t.TempDir(), "dotfiles", "hooks.json")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, hooks); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
			linked := func() {
				t.Helper()
				if info, err := os.Lstat(hooks); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the link was replaced: %v", err)
				}
				if _, err := os.Stat(hooks); err != nil {
					t.Fatalf("the link dangles: %v", err)
				}
			}
			if _, err := m.ApplyUserHooks([]string{"fake"}, true); err != nil {
				t.Fatalf("ApplyUserHooks through a symlink: %v", err)
			}
			linked()
			if data, _ := os.ReadFile(target); !strings.Contains(string(data), "hook --user fake-stop") ||
				strings.Contains(seed, "dark") != strings.Contains(string(data), "dark") {
				t.Fatalf("target after install:\n%s", data)
			}
			if _, err := m.ApplyUserHooks([]string{"fake"}, false); err != nil {
				t.Fatal(err)
			}
			linked()
			if data, _ := os.ReadFile(target); strings.Contains(string(data), "hook --user") {
				t.Fatalf("target after removal:\n%s", data)
			}
		})
	}
}

// Apply and Remove touch git's configuration not at all: the commit hooks live in each
// repository an agent works in (internal/repohooks), never in a hooks path.
func TestSetupLeavesGitsConfigurationAlone(t *testing.T) {
	m, _ := sandbox(t)
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(gitconfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply([]string{"fake"}, func(string) {}, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(func(string) {}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(gitconfig); string(after) != string(before) {
		t.Fatalf("git's global config changed:\n%s", after)
	}
	if entries, _ := os.ReadDir(m.ConfigDir); len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("setup wrote hooks into the config directory: %v", names)
	}
}
