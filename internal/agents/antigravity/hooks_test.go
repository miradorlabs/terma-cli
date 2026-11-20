package antigravity

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

var testCommand = hookmgr.UserHookCommand("/opt/terma/bin/terma")

// agy's hooks.json names each hook at the top level; terma owns one, "terma", and the
// developer's named hooks stay as they were.
func TestAntigravityHooksMergeKeepsOtherNamedHooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{
  "lint": {"PostToolUse": [{"matcher": "run_command", "hooks": [{"command": "./lint.sh"}]}]}
}
`)
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "hooks.json")
	var doc map[string]map[string][]struct {
		Type    string  `json:"type"`
		Command string  `json:"command"`
		Timeout int     `json:"timeout"`
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if len(doc["lint"]["PostToolUse"]) != 1 || doc["lint"]["PostToolUse"][0].Hooks[0].Command != "./lint.sh" {
		t.Fatalf("developer's hook changed:\n%s", got)
	}
	own := doc["terma"]
	if len(own) != len(antigravityHooks) {
		t.Fatalf("terma's events = %v", own)
	}
	for _, h := range antigravityHooks {
		entries := own[h.Event]
		if len(entries) != 1 {
			t.Fatalf("%s: %+v", h.Event, entries)
		}
		command := entries[0].Command
		if h.Grouped {
			if entries[0].Matcher == nil || *entries[0].Matcher != "" || len(entries[0].Hooks) != 1 {
				t.Fatalf("%s should be one group matching every tool: %s", h.Event, got)
			}
			command = entries[0].Hooks[0].Command
		} else if entries[0].Type != "command" || entries[0].Timeout != 10 {
			t.Fatalf("%s handler: %+v", h.Event, entries[0])
		}
		if command != testCommand(h.Hook) {
			t.Errorf("%s runs %q", h.Event, command)
		}
	}
	if _, ok := own["PreToolUse"]; ok {
		t.Error("terma must not gate tools: agy reads a reply without a decision as a denial")
	}
	if again, _ := planUserHooks(root, testCommand, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	if moved, _ := planUserHooks(root, hookmgr.UserHookCommand("/usr/local/bin/terma"), true); moved.Empty() {
		t.Fatal("a moved terma should rewrite its entries")
	}

	un, err := planUserHooks(root, testCommand, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = hookruntest.ReadFile(t, root, "hooks.json")
	if strings.Contains(got, "terma") || !strings.Contains(got, "./lint.sh") {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

// A file terma created holds nothing else once its hook is gone, and goes.
func TestAntigravityHooksCreateAndRemoveFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if un, err := planUserHooks(root, testCommand, false); err != nil || !un.Empty() {
		t.Fatalf("removal from no file = %+v, %v", un, err)
	}
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Action() != "create" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	un, err := planUserHooks(root, testCommand, false)
	if err != nil || len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("uninstall = %+v, %v", un, err)
	}
}

// A hook the developer named "terma" is theirs: setup refuses to replace it and teardown
// leaves it.
func TestAntigravityHooksLeaveADevelopersTermaHook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mine := `{"terma": {"Stop": [{"command": "./my-terma-notes.sh"}]}}`
	hookruntest.WriteFile(t, root, "hooks.json", mine)
	if _, err := planUserHooks(root, testCommand, true); err == nil {
		t.Fatal("install replaced the developer's own hook")
	}
	if un, err := planUserHooks(root, testCommand, false); err != nil || !un.Empty() {
		t.Fatalf("uninstall = %+v, %v", un, err)
	}
}

func TestAntigravityHooksRefuseMalformedFile(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"[]", "{", "null"} {
		root := t.TempDir()
		hookruntest.WriteFile(t, root, "hooks.json", body)
		if _, err := planUserHooks(root, testCommand, true); err == nil {
			t.Errorf("%q: planned over an unreadable file", body)
		}
	}
}

func TestAntigravityUserHooksPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Agent{}.UserHooksPath()
	if err != nil || got != filepath.Join(home, ".gemini", "config", "hooks.json") {
		t.Fatalf("UserHooksPath = %q, %v", got, err)
	}
}
