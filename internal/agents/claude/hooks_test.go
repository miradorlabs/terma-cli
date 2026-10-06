package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// testCommand is a machine-wide hook entry's command, as setup writes it.
var testCommand = hookmgr.UserHookCommand("/opt/terma/bin/terma")

func TestClaudeSettingsMergeKeepsUnknownKeys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "settings.json", `{
  "permissions": {"allow": ["Bash(npm test)"]},
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./lint.sh"}]}]
  }
}
`)
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "settings.json")
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
	if doc.Hooks["PostToolUse"][1].Matcher != "Edit|Write|MultiEdit|NotebookEdit|Agent|Task" || doc.Hooks["PostToolUse"][1].Hooks[0].Command != testCommand("post-tool-use") {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["PostToolUse"][1])
	}
	if len(doc.Hooks["SessionStart"]) != 1 || len(doc.Hooks["SessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if len(doc.Hooks["SubagentStart"]) != 1 || len(doc.Hooks["SubagentStop"]) != 1 || doc.Hooks["SubagentStop"][0].Hooks[0].Command != testCommand("subagent-stop") {
		t.Fatalf("subagent hooks missing: %v", doc.Hooks)
	}
	if again, _ := planUserHooks(root, testCommand, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := planUserHooks(root, testCommand, false)
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = hookruntest.ReadFile(t, root, "settings.json")
	if strings.Contains(got, "terma") || !strings.Contains(got, "./lint.sh") || !strings.Contains(got, "Bash(npm test)") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

func TestClaudeSettingsCreatedAndRemovedWhole(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plan, _ := planUserHooks(root, testCommand, true)
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hookruntest.ReadFile(t, root, "settings.json"), "hook --user session-start") {
		t.Fatal("file not created")
	}
	un, _ := planUserHooks(root, testCommand, false)
	if len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("expected a delete, got %+v", un.Changes)
	}
}

// Hooks files keep `>` and `&` unescaped, the developer's own hooks included.
func TestHookFilesAreNotHTMLEscaped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const userHook = `echo edited >> hooks.log && true`
	hookruntest.WriteFile(t, root, "settings.json", `{
  "hooks": {
    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "`+userHook+`"}]}]
  }
}
`)
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "settings.json")
	if strings.Contains(got, `\u00`) {
		t.Fatalf("HTML-escaped characters in a committed file:\n%s", got)
	}
	// Quotes are JSON's own escapes; only HTML escaping would churn the file.
	verbatim, _ := hookmgr.MarshalJSON(testCommand("session-start"), "", "")
	if !strings.Contains(got, string(verbatim)) || !strings.Contains(got, "&&") {
		t.Fatalf("guarded command not written verbatim:\n%s", got)
	}
	if !strings.Contains(got, userHook) {
		t.Fatalf("user's redirecting hook was rewritten:\n%s", got)
	}
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
	if doc.Hooks["SessionStart"][0].Hooks[0].Command != testCommand("session-start") {
		t.Fatalf("terma hook parsed as %q", doc.Hooks["SessionStart"][0].Hooks[0].Command)
	}
	_ = os.Remove // keep os imported for readers extending this test with file checks
}

// A first install still creates what is missing.
func TestPlannersStillCreateWhatIsMissing(t *testing.T) {
	t.Parallel()
	plan, err := planUserHooks(t.TempDir(), testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Before != nil {
		t.Fatalf("a missing settings file should be one create, got %+v", plan.Changes)
	}
}

// Managed settings carry one entry per event, calling terma by the managed path.
func TestManagedSettings(t *testing.T) {
	cmd := hookmgr.ManagedHookCommand("$HOME/.local/bin/terma")
	claude, err := managedSettings(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(claude, &settings); err != nil || len(settings.Hooks) != len(claudeHooks) {
		t.Fatalf("managed settings: %v, %d events\n%s", err, len(settings.Hooks), claude)
	}
}
