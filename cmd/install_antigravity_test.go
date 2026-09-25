package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
)

func TestInstallWiresAntigravityHooksWhenAsked(t *testing.T) {
	repo := installRepo(t)
	out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,antigravity", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, ".agents/hooks.json") {
		t.Errorf("plan did not list Antigravity's hooks file:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".agents", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json not written: %v", err)
	}
	var doc map[string]struct {
		PreInvocation  []map[string]any `json:"PreInvocation"`
		PostInvocation []map[string]any `json:"PostInvocation"`
		Stop           []map[string]any `json:"Stop"`
		PostToolUse    []struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"PostToolUse"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	terma, ok := doc["terma"]
	if !ok || len(doc) != 1 {
		t.Fatalf("expected exactly terma's named hook, got %v", doc)
	}
	for event, entries := range map[string][]map[string]any{
		"PreInvocation": terma.PreInvocation, "PostInvocation": terma.PostInvocation, "Stop": terma.Stop,
	} {
		if len(entries) != 1 || entries[0]["command"] != hookmgr.HookCommand("antigravity-"+strings.ToLower(strings.ReplaceAll(event, "Invocation", "-invocation"))) {
			t.Errorf("%s = %v", event, entries)
		}
	}
	if len(terma.PostToolUse) != 1 || terma.PostToolUse[0].Matcher != "" || terma.PostToolUse[0].Hooks[0]["command"] != hookmgr.HookCommand("antigravity-post-tool-use") {
		t.Errorf("PostToolUse = %+v", terma.PostToolUse)
	}
	if got := strings.Join(adapter.WiredNames(repo), ","); got != "claude,antigravity" {
		t.Errorf("wired adapters = %q, want claude,antigravity", got)
	}

	out, err = runTerma(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "hooks.json")); err == nil {
		t.Error("hooks.json survived uninstall")
	}
}

// A repository with one of agy's customization directories is one people open in
// Antigravity, so its hooks come along by default; one without is left alone.
func TestInstallWiresAntigravityByDefaultOnlyWhereItIsUsed(t *testing.T) {
	repo := installRepo(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "hooks.json")); err == nil {
		t.Fatal("a repository with no .agents directory got Antigravity hooks by default")
	}
	if out, err := runTerma(t, "uninstall", "--yes"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agents", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "hooks.json")); err != nil {
		t.Fatal("a repository with a .agents directory should get Antigravity hooks by default")
	}
	if !slices.Contains(adapter.WiredNames(repo), "antigravity") {
		t.Errorf("antigravity is not wired: %v", adapter.WiredNames(repo))
	}
}

// agy loads a workspace's hooks only once the developer has trusted the workspace from
// inside agy, and records that in its own settings. Until then the committed file is
// inert and only doctor can say so.
func TestDoctorReportsAntigravityWorkspaceTrust(t *testing.T) {
	repo := installRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,antigravity", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "Antigravity hooks present") || !strings.Contains(out, "not a trusted Antigravity workspace") {
		t.Fatalf("doctor should see the hooks and the missing trust:\n%s", out)
	}

	settings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"trustedWorkspaces": ["`+repo+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ = runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "Antigravity hooks present and the workspace is trusted") {
		t.Fatalf("doctor should report the trusted workspace:\n%s", out)
	}

	// The developer's own off switch is respected by install and reported by doctor.
	hooks := filepath.Join(repo, ".agents", "hooks.json")
	data, _ := os.ReadFile(hooks)
	data = []byte(strings.Replace(string(data), `"terma": {`, `"terma": {"enabled": false,`, 1))
	if err := os.WriteFile(hooks, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTerma(t, "doctor", "--skip-commit")
	if !strings.Contains(out, "switched off") {
		t.Fatalf("doctor should report the disabled entry:\n%s", out)
	}
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,antigravity", "--yes"); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(hooks); !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("reinstall switched the developer's hooks back on:\n%s", data)
	}
}

func TestDoctorIgnoresAntigravityWithoutTheAdapter(t *testing.T) {
	installRepo(t)
	t.Setenv("HOME", t.TempDir())
	if _, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "Antigravity") {
		t.Fatalf("doctor mentioned Antigravity in a repository that has none of its hooks:\n%s", out)
	}
}
