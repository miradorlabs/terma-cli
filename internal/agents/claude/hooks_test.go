package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
)

func TestClaudeSettingsMergeKeepsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	hookruntest.WriteFile(t, root, settingsPath, `{
  "permissions": {"allow": ["Bash(npm test)"]},
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./lint.sh"}]}]
  }
}
`)
	plan, err := planSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, settingsPath)
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
	if doc.Hooks["PostToolUse"][1].Matcher != "Edit|Write|MultiEdit|NotebookEdit|Agent|Task" || doc.Hooks["PostToolUse"][1].Hooks[0].Command != hookmgr.HookCommand("post-tool-use") {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["PostToolUse"][1])
	}
	if len(doc.Hooks["SessionStart"]) != 1 || len(doc.Hooks["SessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if len(doc.Hooks["SubagentStart"]) != 1 || len(doc.Hooks["SubagentStop"]) != 1 || doc.Hooks["SubagentStop"][0].Hooks[0].Command != hookmgr.HookCommand("subagent-stop") {
		t.Fatalf("subagent hooks missing: %v", doc.Hooks)
	}
	if again, _ := planSettings(root, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := planSettings(root, false)
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = hookruntest.ReadFile(t, root, settingsPath)
	if strings.Contains(got, "terma") || !strings.Contains(got, "./lint.sh") || !strings.Contains(got, "Bash(npm test)") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

func TestClaudeSettingsCreatedAndRemovedWhole(t *testing.T) {
	root := t.TempDir()
	plan, _ := planSettings(root, true)
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hookruntest.ReadFile(t, root, settingsPath), "terma hook session-start") {
		t.Fatal("file not created")
	}
	un, _ := planSettings(root, false)
	if len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("expected a delete, got %+v", un.Changes)
	}
}

// The committed files hold the guard as written. encoding/json would otherwise commit
// `>` and `&` as \u003e and \u0026 — and rewrite any user hook that carries a redirect
// the same way.
func TestHookFilesAreNotHTMLEscaped(t *testing.T) {
	root := t.TempDir()
	const userHook = `echo edited >> hooks.log && true`
	hookruntest.WriteFile(t, root, settingsPath, `{
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "`+userHook+`"}]}]
  }
}
`)
	plan, err := planSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, settingsPath)
	if strings.Contains(got, `\u00`) {
		t.Fatalf("HTML-escaped characters in a committed file:\n%s", got)
	}
	if !strings.Contains(got, hookmgr.HookCommand("session-start")) {
		t.Fatalf("guarded command not written verbatim:\n%s", got)
	}
	if !strings.Contains(got, userHook) {
		t.Fatalf("user's redirecting hook was rewritten:\n%s", got)
	}
	// Still valid JSON that parses back to the same commands.
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if doc.Hooks["PostToolUse"][0].Hooks[0].Command != userHook {
		t.Fatalf("user hook parsed as %q", doc.Hooks["PostToolUse"][0].Hooks[0].Command)
	}
	if doc.Hooks["SessionStart"][0].Hooks[0].Command != hookmgr.HookCommand("session-start") {
		t.Fatalf("terma hook parsed as %q", doc.Hooks["SessionStart"][0].Hooks[0].Command)
	}
	_ = os.Remove // keep os imported for readers extending this test with file checks
}

// Absent is still absent: the contract change must not turn a first install into an error.
func TestPlannersStillCreateWhatIsMissing(t *testing.T) {
	plan, err := planSettings(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Before != nil {
		t.Fatalf("a missing settings file should be one create, got %+v", plan.Changes)
	}
}

// Managed settings carry one entry per committed event, calling terma by the managed path.
func TestManagedSettings(t *testing.T) {
	cmd := hookmgr.ManagedHookCommand("$HOME/.local/bin/terma")
	claude, err := managedSettings(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(claude, &settings); err != nil || len(settings.Hooks) != len(committedHooks) {
		t.Fatalf("managed settings: %v, %d events\n%s", err, len(settings.Hooks), claude)
	}
}
