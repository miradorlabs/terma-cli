package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
)

func codexHooksIn(t *testing.T, repo string) map[string][]struct {
	Matcher *string `json:"matcher"`
	Hooks   []struct {
		Command string `json:"command"`
		Async   bool   `json:"async"`
	} `json:"hooks"`
} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(hookmgr.CodexHooksPath)))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	return doc.Hooks
}

func TestInstallWiresCodexHooksWhenAsked(t *testing.T) {
	repo := installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	hooks := codexHooksIn(t, repo)
	for _, want := range []struct{ event, command string }{
		{"SessionStart", hookmgr.HookCommand("codex-session-start")},
		{"PostToolUse", hookmgr.HookCommand("codex-post-tool-use")},
		{"SessionEnd", hookmgr.HookCommand("codex-session-end")},
	} {
		groups := hooks[want.event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != want.command {
			t.Fatalf("%s not wired: %+v", want.event, groups)
		}
	}
	if !strings.Contains(out, hookmgr.CodexHooksPath) {
		t.Fatalf("the plan should name the file it writes:\n%s", out)
	}
}

// A repository nobody opens in Codex should not gain a .codex directory.
func TestInstallSkipsCodexHooksByDefault(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(hookmgr.CodexHooksPath))); !os.IsNotExist(err) {
		t.Fatal("Codex hooks written into a repository that has no .codex directory")
	}
}

// ...but one that already has Codex configuration gets them without being asked.
func TestInstallWiresCodexHooksWhereCodexIsUsed(t *testing.T) {
	repo := installRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(codexHooksIn(t, repo)["PostToolUse"]) != 1 {
		t.Fatal("a repository with a .codex directory should get the hooks by default")
	}
}

func TestUninstallRemovesCodexHooks(t *testing.T) {
	repo := installRepo(t)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,codex", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runTerma(t, "uninstall", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(hookmgr.CodexHooksPath))); !os.IsNotExist(err) {
		t.Fatal("uninstall left Codex hooks behind")
	}
}
