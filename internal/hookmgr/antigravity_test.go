package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agy's hooks.json is keyed by hook name. terma owns one name and leaves every other
// author's entry byte-for-byte.
func TestAntigravityHooksMergeKeepsOtherNamedHooks(t *testing.T) {
	root := t.TempDir()
	write(t, root, AntigravityHooksPath, `{
  "lint-checker": {
    "PostToolUse": [{"matcher": "run_command", "hooks": [{"type": "command", "command": "./scripts/lint.sh", "timeout": 10}]}]
  }
}
`)
	plan, err := PlanAntigravityHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, AntigravityHooksPath)
	var doc map[string]struct {
		Enabled        *bool `json:"enabled"`
		PreInvocation  []struct{ Command string }
		PostInvocation []struct{ Command string }
		Stop           []struct{ Command string }
		PostToolUse    []struct {
			Matcher string
			Hooks   []struct {
				Type    string
				Command string
				Timeout int
			}
		}
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if len(doc) != 2 || doc["lint-checker"].PostToolUse[0].Hooks[0].Command != "./scripts/lint.sh" {
		t.Fatalf("other author's hook lost:\n%s", got)
	}
	terma := doc[antigravityHookName]
	if terma.Enabled != nil {
		t.Fatalf("terma must not set enabled on a fresh entry:\n%s", got)
	}
	if len(terma.PreInvocation) != 1 || terma.PreInvocation[0].Command != HookCommand("antigravity-pre-invocation") {
		t.Fatalf("PreInvocation wrong: %+v", terma.PreInvocation)
	}
	if len(terma.PostInvocation) != 1 || len(terma.Stop) != 1 || terma.Stop[0].Command != HookCommand("antigravity-stop") {
		t.Fatalf("flat events wrong:\n%s", got)
	}
	ptu := terma.PostToolUse
	if len(ptu) != 1 || ptu[0].Matcher != "" || len(ptu[0].Hooks) != 1 || ptu[0].Hooks[0].Command != HookCommand("antigravity-post-tool-use") || ptu[0].Hooks[0].Type != "command" || ptu[0].Hooks[0].Timeout != 10 {
		t.Fatalf("PostToolUse group wrong: %+v", ptu)
	}
	if again, _ := PlanAntigravityHooks(root, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := PlanAntigravityHooks(root, false)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = read(t, root, AntigravityHooksPath)
	if strings.Contains(got, "terma") || !strings.Contains(got, "./scripts/lint.sh") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

// A developer who switched terma's hooks off keeps them off through a reinstall; a
// fresh file and a re-enabled entry carry no switch at all.
func TestAntigravityHooksPreserveTheEnabledSwitch(t *testing.T) {
	root := t.TempDir()
	write(t, root, AntigravityHooksPath, `{"terma": {"enabled": false, "Stop": [{"type": "command", "command": "terma hook antigravity-stop", "timeout": 10}]}}`)
	if AntigravityHooksEnabled(root) {
		t.Fatal("enabled: false should read as switched off")
	}
	plan, err := PlanAntigravityHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("a stale, unguarded entry must be upgraded")
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, AntigravityHooksPath)
	if !strings.Contains(got, `"enabled": false`) {
		t.Fatalf("the developer's switch was lost:\n%s", got)
	}
	if strings.Contains(got, `"terma hook`) || !strings.Contains(got, HookCommand("antigravity-stop")) {
		t.Fatalf("stale command not upgraded in place:\n%s", got)
	}
	if AntigravityHooksEnabled(root) {
		t.Fatal("still switched off after the upgrade")
	}
	if AntigravityHooksEnabled(t.TempDir()) {
		// No file at all is not "switched off": presence is the plan's question.
	} else {
		t.Fatal("a missing file must not read as switched off")
	}
}

// A file terma creates holds only its own entry and goes away whole on uninstall.
func TestAntigravityHooksCreatedAndRemovedWhole(t *testing.T) {
	root := t.TempDir()
	plan, _ := PlanAntigravityHooks(root, true)
	if len(plan.Changes) != 1 || plan.Changes[0].Action() != "create" || plan.Changes[0].Path != AntigravityHooksPath {
		t.Fatalf("expected a create of %s, got %+v", AntigravityHooksPath, plan.Changes)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, AntigravityHooksPath)
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &top); err != nil || len(top) != 1 {
		t.Fatalf("file should hold exactly terma's entry (%v):\n%s", err, got)
	}
	if strings.Contains(got, `\u00`) {
		t.Fatalf("HTML-escaped guard in a committed file:\n%s", got)
	}
	un, _ := PlanAntigravityHooks(root, false)
	if len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("expected a delete, got %+v", un.Changes)
	}
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); err == nil {
		t.Fatal("an empty .agents directory terma created was left behind")
	}
	if again, _ := PlanAntigravityHooks(root, false); !again.Empty() {
		t.Fatal("uninstall with nothing installed should plan nothing")
	}
}

func TestAntigravityHooksRefusesMalformedFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, AntigravityHooksPath, `{"terma": [`)
	if _, err := PlanAntigravityHooks(root, true); err == nil {
		t.Fatal("a file terma cannot parse must not be rewritten")
	}
}

// agy accepts four spellings of its customization root; any one of them means the
// repository is opened in Antigravity.
func TestHasAntigravity(t *testing.T) {
	for _, dir := range []string{".agents", ".agent", "_agents", "_agent"} {
		root := t.TempDir()
		if HasAntigravity(root) {
			t.Fatal("no customization root yet")
		}
		if err := os.WriteFile(filepath.Join(root, dir), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if HasAntigravity(root) {
			t.Fatalf("a file named %s is not a customization root", dir)
		}
		_ = os.Remove(filepath.Join(root, dir))
		if err := os.MkdirAll(filepath.Join(root, dir, "rules"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !HasAntigravity(root) {
			t.Fatalf("%s/ should mark the repository as used with Antigravity", dir)
		}
	}
}
