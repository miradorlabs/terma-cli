package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	if d := Detect(root); d.Manager != GitShim {
		t.Fatalf("empty repo should fall back to the shim, got %s", d.Manager)
	}
	write(t, root, "package.json", `{"devDependencies":{"husky":"^9"}}`)
	if d := Detect(root); d.Manager != Husky {
		t.Fatalf("husky in package.json, got %s", d.Manager)
	}
	write(t, root, "lefthook.yml", "pre-commit:\n  commands: {}\n")
	if d := Detect(root); d.Manager != Lefthook || d.ConfigPath != "lefthook.yml" {
		t.Fatalf("lefthook.yml should win over package.json, got %+v", d)
	}
	write(t, root, ".husky/pre-commit", "npm test\n")
	if d := Detect(root); d.Manager != Husky {
		t.Fatalf(".husky/ should win, got %s", d.Manager)
	}
}

func TestShimPlanRoundTrip(t *testing.T) {
	root := t.TempDir()
	det := Detect(root)
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Action() != "create" {
		t.Fatalf("unexpected plan: %+v", plan.Changes)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	script := read(t, root, ShimDir+"/prepare-commit-msg")
	for _, want := range []string{"#!/bin/sh", `terma hook prepare-commit-msg "$@" || true`, "command -v terma", "git rev-parse --git-common-dir", `-ef "$0"`} {
		if !strings.Contains(script, want) {
			t.Errorf("shim missing %q", want)
		}
	}
	info, _ := os.Stat(filepath.Join(root, ShimDir, "post-commit"))
	if info.Mode()&0o111 == 0 {
		t.Fatal("shim must be executable")
	}
	// Idempotent.
	again, _ := PlanInstall(root, det)
	if !again.Empty() {
		t.Fatalf("second install should change nothing: %+v", again.Changes)
	}
	// Symmetric.
	un, _ := PlanUninstall(root, det)
	if len(un.Changes) != 2 || un.Changes[0].Action() != "delete" {
		t.Fatalf("unexpected uninstall plan: %+v", un.Changes)
	}
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".terma")); !os.IsNotExist(err) {
		t.Fatal("empty shim dir should be removed")
	}
}

func TestHuskyPreservesUserLines(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".husky/prepare-commit-msg", "#!/bin/sh\nnpx commitlint --edit \"$1\"\n")
	det := Detection{Manager: Husky}
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, ".husky/prepare-commit-msg")
	if !strings.HasPrefix(got, "#!/bin/sh\nnpx commitlint --edit \"$1\"\n") || !strings.Contains(got, `terma hook prepare-commit-msg "$@" || true`) {
		t.Fatalf("user line lost or terma line missing:\n%s", got)
	}
	if !strings.Contains(read(t, root, ".husky/post-commit"), "terma hook post-commit") {
		t.Fatal("post-commit hook not created")
	}
	un, _ := PlanUninstall(root, det)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, ".husky/prepare-commit-msg"); got != "#!/bin/sh\nnpx commitlint --edit \"$1\"\n" {
		t.Fatalf("uninstall should leave only the user's lines:\n%s", got)
	}
	if read(t, root, ".husky/post-commit") != "" {
		t.Fatal("a hook file terma created alone should be removed")
	}
}

func TestLefthookMergesIntoExistingConfig(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lefthook.yml", "# team config\npre-commit:\n  commands:\n    lint:\n      run: npm run lint\n")
	det := Detection{Manager: Lefthook, ConfigPath: "lefthook.yml"}
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, "lefthook.yml")
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# team config") || !strings.Contains(got, "npm run lint") {
		t.Fatalf("existing content lost:\n%s", got)
	}
	pcm := doc["prepare-commit-msg"].(map[string]any)["commands"].(map[string]any)["terma"].(map[string]any)
	if run := pcm["run"].(string); !strings.Contains(run, "terma hook prepare-commit-msg {1} {2} {3}") {
		t.Fatalf("unexpected run: %s", run)
	}
	if again, _ := PlanInstall(root, det); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := PlanUninstall(root, det)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = read(t, root, "lefthook.yml")
	if strings.Contains(got, "terma") || !strings.Contains(got, "npm run lint") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

func TestPreCommitAddsLocalHooksAndHookTypes(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".pre-commit-config.yaml", "repos:\n  - repo: https://github.com/psf/black\n    rev: 24.0.0\n    hooks:\n      - id: black\n")
	det := Detection{Manager: PreCommit, ConfigPath: ".pre-commit-config.yaml"}
	plan, err := PlanInstall(root, det)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, ".pre-commit-config.yaml")
	var doc struct {
		Types []string `yaml:"default_install_hook_types"`
		Repos []struct {
			Repo  string `yaml:"repo"`
			Hooks []struct {
				ID     string   `yaml:"id"`
				Stages []string `yaml:"stages"`
			} `yaml:"hooks"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Repos) != 2 || doc.Repos[0].Repo != "https://github.com/psf/black" || doc.Repos[1].Repo != "local" {
		t.Fatalf("unexpected repos: %+v", doc.Repos)
	}
	if len(doc.Repos[1].Hooks) != 2 || doc.Repos[1].Hooks[0].ID != "terma-prepare-commit-msg" || doc.Repos[1].Hooks[0].Stages[0] != "prepare-commit-msg" {
		t.Fatalf("unexpected hooks: %+v", doc.Repos[1].Hooks)
	}
	if strings.Join(doc.Types, ",") != "pre-commit,prepare-commit-msg,post-commit" {
		t.Fatalf("hook types: %v", doc.Types)
	}
	un, _ := PlanUninstall(root, det)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, ".pre-commit-config.yaml"); strings.Contains(got, "terma") || !strings.Contains(got, "psf/black") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

func TestClaudeSettingsMergeKeepsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	write(t, root, ClaudeSettingsPath, `{
  "permissions": {"allow": ["Bash(npm test)"]},
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./lint.sh"}]}]
  }
}
`)
	plan, err := PlanClaudeSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, ClaudeSettingsPath)
	var doc struct {
		Permissions json.RawMessage `json:"permissions"`
		Hooks       map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if string(doc.Permissions) != `{"allow": ["Bash(npm test)"]}` {
		t.Fatalf("permissions rewritten: %s", doc.Permissions)
	}
	if len(doc.Hooks["PostToolUse"]) != 2 || doc.Hooks["PostToolUse"][0].Hooks[0].Command != "./lint.sh" {
		t.Fatalf("existing PostToolUse hook lost: %+v", doc.Hooks["PostToolUse"])
	}
	if doc.Hooks["PostToolUse"][1].Matcher != "Edit|Write|MultiEdit|NotebookEdit|Agent|Task" || doc.Hooks["PostToolUse"][1].Hooks[0].Command != HookCommand("post-tool-use") {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["PostToolUse"][1])
	}
	if len(doc.Hooks["SessionStart"]) != 1 || len(doc.Hooks["SessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if len(doc.Hooks["SubagentStart"]) != 1 || len(doc.Hooks["SubagentStop"]) != 1 || doc.Hooks["SubagentStop"][0].Hooks[0].Command != HookCommand("subagent-stop") {
		t.Fatalf("subagent hooks missing: %v", doc.Hooks)
	}
	if again, _ := PlanClaudeSettings(root, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := PlanClaudeSettings(root, false)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = read(t, root, ClaudeSettingsPath)
	if strings.Contains(got, "terma") || !strings.Contains(got, "./lint.sh") || !strings.Contains(got, "Bash(npm test)") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

func TestClaudeSettingsCreatedAndRemovedWhole(t *testing.T) {
	root := t.TempDir()
	plan, _ := PlanClaudeSettings(root, true)
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, root, ClaudeSettingsPath), "terma hook session-start") {
		t.Fatal("file not created")
	}
	un, _ := PlanClaudeSettings(root, false)
	if len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("expected a delete, got %+v", un.Changes)
	}
}
