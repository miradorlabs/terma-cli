package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
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
	if doc.Hooks["PostToolUse"][1].Matcher != "Edit|Write|MultiEdit|NotebookEdit|Agent|Task" || doc.Hooks["PostToolUse"][1].Hooks[0].Command != hookmgr.PathHookCommand("post-tool-use") {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["PostToolUse"][1])
	}
	if len(doc.Hooks["SessionStart"]) != 1 || len(doc.Hooks["SessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if len(doc.Hooks["SubagentStart"]) != 1 || len(doc.Hooks["SubagentStop"]) != 1 || doc.Hooks["SubagentStop"][0].Hooks[0].Command != hookmgr.PathHookCommand("subagent-stop") {
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

// A Claude started from the Dock or an IDE has only the system PATH, and still reaches a
// terma installed under the home directory.
func TestClaudeHookCommandFindsHomeInstallWithGUIPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook command uses a POSIX shell")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "terma"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/invoked\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var command string
	for _, h := range committedHooks {
		if h.Event == "SessionStart" {
			command = h.Command
		}
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook command failed: %v, output %q", err, out)
	}
	got, err := os.ReadFile(filepath.Join(home, "invoked"))
	if err != nil || string(got) != "hook\nsession-start\n" {
		t.Fatalf("home install not invoked: %q, %v", got, err)
	}
}

// A file an earlier terma wrote, calling terma by name alone, is rewritten in place: the
// old entry goes rather than running beside the new one.
func TestClaudeSettingsUpgradeReplacesBarePATHEntries(t *testing.T) {
	root := t.TempDir()
	old, err := json.Marshal(hookmgr.HookCommand("session-start"))
	if err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, settingsPath, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":`+string(old)+`,"timeout":10}]}]}}`)
	plan, err := planSettings(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("an install over the bare-PATH entries changed nothing")
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, settingsPath)
	if strings.Count(got, "terma hook session-start") != 1 || !strings.Contains(got, `$HOME/.local/bin`) {
		t.Fatalf("SessionStart not upgraded in place:\n%s", got)
	}
	plan, err = planSettings(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, settingsPath)); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the settings file: %v", err)
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

// Committed files keep `>` and `&` unescaped, user hooks included.
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
	// Quotes are JSON's own escapes; only HTML escaping would churn the file.
	verbatim, _ := hookmgr.MarshalJSON(hookmgr.PathHookCommand("session-start"), "", "")
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
	if doc.Hooks["SessionStart"][0].Hooks[0].Command != hookmgr.PathHookCommand("session-start") {
		t.Fatalf("terma hook parsed as %q", doc.Hooks["SessionStart"][0].Hooks[0].Command)
	}
	_ = os.Remove // keep os imported for readers extending this test with file checks
}

// A first install still creates what is missing.
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
