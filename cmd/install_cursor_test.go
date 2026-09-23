package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// installAdapters unions the committed set, the configured agents, and directory-present
// agents — so a selection grows the wired hooks and a narrower re-run never removes them.
func TestInstallAdaptersUnionGrowsNeverShrinks(t *testing.T) {
	root := t.TempDir()
	existing := &termaproject.File{Install: termaproject.Install{Adapters: []string{"claude"}}}

	// Selecting more agents wires their committed hooks too (opencode has no hooks file).
	got := installAdapters(root, []string{"claude", "cursor", "codex", "opencode"}, "", existing)
	if strings.Join(got, ",") != "claude,cursor,codex" {
		t.Fatalf("selecting agents should grow adapters, got %v", got)
	}
	// A narrower re-run preserves what was already committed (no churn-down).
	wide := &termaproject.File{Install: termaproject.Install{Adapters: []string{"claude", "cursor", "codex"}}}
	if got := installAdapters(root, []string{"claude"}, "", wide); strings.Join(got, ",") != "claude,cursor,codex" {
		t.Fatalf("a narrower re-run must not drop committed adapters, got %v", got)
	}
	// --adapters overrides outright.
	if got := installAdapters(root, []string{"claude", "cursor"}, "codex", existing); strings.Join(got, ",") != "codex" {
		t.Fatalf("--adapters should override, got %v", got)
	}
}

// installRepo is a fresh git repository the test runs from, with Terma's own directory
// sandboxed so nothing here can read a real credential — which is also what makes the
// project id below get accepted verbatim instead of resolved against an organization.
func installRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "user.email", "dev@example.com"}, {"-C", repo, "config", "user.name", "Dev"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	// Sandbox Claude's config too, so a test that ever wires Claude (which would wrap the
	// status line) never touches the developer's real ~/.claude/settings.json.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(repo)
	realDir, _ := filepath.EvalSymlinks(repo)
	return realDir
}

const testProjectID = "770e8400-e29b-41d4-a716-446655440000"

func TestInstallWiresCursorHooksWhenAsked(t *testing.T) {
	repo := installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,cursor", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, ".cursor/hooks.json") {
		t.Errorf("plan did not list Cursor's hooks file:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json not written: %v", err)
	}
	var doc struct {
		Version int                         `json:"version"`
		Hooks   map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	for event, command := range map[string]string{
		"sessionStart":       hookmgr.HookCommand("cursor-session-start"),
		"sessionEnd":         hookmgr.HookCommand("cursor-session-end"),
		"afterFileEdit":      hookmgr.HookCommand("cursor-file-edit"),
		"postToolUse":        hookmgr.HookCommand("cursor-post-tool-use"),
		"postToolUseFailure": hookmgr.HookCommand("cursor-post-tool-use-failure"),
	} {
		entries := doc.Hooks[event]
		if len(entries) != 1 || entries[0]["command"] != command {
			t.Errorf("%s = %v, want one entry running %q", event, entries, command)
		}
	}
	binding, _ := os.ReadFile(filepath.Join(repo, ".terma", "settings.json"))
	if !strings.Contains(string(binding), `"claude"`) || !strings.Contains(string(binding), `"cursor"`) {
		t.Errorf(".terma/settings.json does not record the cursor adapter:\n%s", binding)
	}

	// Symmetric: uninstall removes the file terma created.
	out, err = runTerma(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); err == nil {
		t.Error("hooks.json survived uninstall")
	}
}

// A repository that already has a .cursor directory is one people open in Cursor, so
// its hooks come along by default; one without is left alone.
func TestInstallWiresCursorByDefaultOnlyWhereCursorIsUsed(t *testing.T) {
	repo := installRepo(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); err == nil {
		t.Fatal("a repository with no .cursor directory got Cursor hooks by default")
	}
	if out, err := runTerma(t, "uninstall", "--yes"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}

	if err := os.MkdirAll(filepath.Join(repo, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); err != nil {
		t.Fatal("a repository with a .cursor directory should get Cursor hooks by default")
	}
	binding, _ := os.ReadFile(filepath.Join(repo, ".terma", "settings.json"))
	if !strings.Contains(string(binding), "cursor") {
		t.Errorf(".terma/settings.json does not record the cursor adapter:\n%s", binding)
	}
}

func TestInstallRejectsUnknownAdapter(t *testing.T) {
	installRepo(t)
	_, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "zed", "--yes")
	if err == nil || !strings.Contains(err.Error(), "zed") || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v", err)
	}
	// The message is the only place a reader learns what they could have typed, so it
	// has to keep naming every adapter that writes a repo-scope file.
	for _, adapter := range []string{"claude", "cursor", "codex"} {
		if !strings.Contains(err.Error(), adapter) {
			t.Fatalf("error should offer %q: %v", adapter, err)
		}
	}
}

// A colleague who clones an already-onboarded repo and runs `terma install` must not
// churn the committed binding: installed_at/terma_version are preserved and the wired
// adapters stay what the repo already committed, so re-running produces no diff.
func TestInstallReRunDoesNotChurnBinding(t *testing.T) {
	repo := installRepo(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	path := filepath.Join(repo, ".terma", "settings.json")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Re-run with no --adapters, as a second developer would.
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("re-install churned the committed binding:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(second), `"claude"`) {
		t.Fatalf("committed adapter was lost on re-install:\n%s", second)
	}
}
