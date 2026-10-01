package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
)

// install.Adapters unions wired, configured and directory-present agents, so a narrower
// re-run never removes hooks; coming-soon agents are left out of all three.
func TestInstallAdaptersUnionGrowsNeverShrinks(t *testing.T) {
	root := t.TempDir()

	// Selecting more agents wires their committed hooks too.
	got := install.Adapters(testApp.agents, root, []string{"claude", "cursor", "codex", "opencode"}, nil)
	if strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("selecting agents should grow adapters, got %v", got)
	}
	// A narrower re-run keeps what a colleague committed.
	wireAdapters(t, root, "codex")
	if got := install.Adapters(testApp.agents, root, []string{"claude"}, nil); strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("a narrower re-run must not drop committed adapters, got %v", got)
	}
	if got := install.Adapters(testApp.agents, root, []string{"claude", "cursor"}, splitCommas("codex")); strings.Join(got, ",") != "codex" {
		t.Fatalf("--adapters should override, got %v", got)
	}
}

// Coming-soon agents get no hooks from install, whether the repository carries their
// directory or a colleague committed their file; only --adapters wires one.
func TestInstallAdaptersLeavesComingSoonAgentsOut(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".cursor", ".agents"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wireAdapters(t, root, "cursor", "antigravity")
	if got := install.Adapters(testApp.agents, root, []string{"claude"}, nil); strings.Join(got, ",") != "claude" {
		t.Fatalf("a coming-soon agent was wired by default: %v", got)
	}
	if got := install.Adapters(testApp.agents, root, []string{"claude"}, splitCommas("claude,cursor")); strings.Join(got, ",") != "claude,cursor" {
		t.Fatalf("--adapters cursor should still wire Cursor, got %v", got)
	}
}

// wireAdapters writes the named adapters' hooks files into root, as a colleague would have.
func wireAdapters(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		a, ok := testApp.agents.Lookup(name)
		if !ok {
			t.Fatalf("no adapter %q", name)
		}
		plan, err := a.Plan(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(root, plan); err != nil {
			t.Fatal(err)
		}
	}
}

// installRepo is a fresh git repository with Terma's directory sandboxed, so no real
// credential is read and the project id is accepted verbatim.
func installRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "user.email", "dev@example.com"}, {"-C", repo, "config", "user.name", "Dev"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	// Sandboxed so a wired status line never touches the developer's real settings.
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
	if got := strings.Join(testApp.agents.WiredNames(repo), ","); got != "claude,cursor" {
		t.Errorf("wired adapters = %q, want claude,cursor", got)
	}

	// Uninstall removes commands, retaining the schema version whose ownership is ambiguous.
	out, err = runTerma(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	remaining, err := os.ReadFile(filepath.Join(repo, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(remaining), "terma hook") || !strings.Contains(string(remaining), "version") {
		t.Fatalf("uninstall: %s", remaining)
	}
}

// A coming-soon agent's directory in the repository gets no hooks from a plain install,
// and the plan does not name its file.
func TestInstallLeavesCursorAloneWhileComingSoon(t *testing.T) {
	repo := installRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes", "--verbose")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cursor", "hooks.json")); err == nil {
		t.Fatal("a repository with a .cursor directory got Cursor hooks by default")
	}
	if strings.Contains(out, ".cursor") || slices.Contains(testApp.agents.WiredNames(repo), "cursor") {
		t.Fatalf("install offered Cursor's hooks:\n%s", out)
	}
}

func TestInstallRejectsUnknownAdapter(t *testing.T) {
	installRepo(t)
	_, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "zed", "--yes")
	if err == nil || !strings.Contains(err.Error(), "zed") || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v", err)
	}
	// The message must keep naming every adapter that writes a repo-scope file.
	for _, adapter := range []string{"claude", "cursor", "codex"} {
		if !strings.Contains(err.Error(), adapter) {
			t.Fatalf("error should offer %q: %v", adapter, err)
		}
	}
}

// Re-running install in an onboarded repository leaves the committed binding unchanged.
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
	if !slices.Contains(testApp.agents.WiredNames(repo), "claude") {
		t.Fatalf("committed adapter was lost on re-install: %v", testApp.agents.WiredNames(repo))
	}
	// A developer's extra agent gets its own hooks file; the binding stays the team's.
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,cursor", "--yes"); err != nil {
		t.Fatalf("re-install with cursor: %v\n%s", err, out)
	}
	third, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, third) {
		t.Fatalf("another developer's agents churned the committed binding:\n--- first ---\n%s\n--- third ---\n%s", first, third)
	}
	if got := strings.Join(testApp.agents.WiredNames(repo), ","); got != "claude,cursor" {
		t.Fatalf("wired adapters = %q, want claude,cursor", got)
	}
}
