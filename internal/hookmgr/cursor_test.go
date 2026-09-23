package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorHooksMergeKeepsUnknownKeysAndUserHooks(t *testing.T) {
	root := t.TempDir()
	write(t, root, CursorHooksPath, `{
  "version": 1,
  "hooks": {
    "afterFileEdit": [{"command": "./format.sh"}],
    "beforeShellExecution": [{"command": "./audit.sh"}]
  }
}
`)
	plan, err := PlanCursorHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, CursorHooksPath)
	var doc struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, got)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d", doc.Version)
	}
	if len(doc.Hooks["afterFileEdit"]) != 2 || doc.Hooks["afterFileEdit"][0].Command != "./format.sh" {
		t.Fatalf("user's afterFileEdit hook lost: %+v", doc.Hooks["afterFileEdit"])
	}
	if doc.Hooks["afterFileEdit"][1].Command != HookCommand("cursor-file-edit") || doc.Hooks["afterFileEdit"][1].Timeout != 10 {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["afterFileEdit"][1])
	}
	if len(doc.Hooks["beforeShellExecution"]) != 1 {
		t.Fatalf("unrelated hook touched: %+v", doc.Hooks["beforeShellExecution"])
	}
	if len(doc.Hooks["sessionStart"]) != 1 || len(doc.Hooks["sessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if again, _ := PlanCursorHooks(root, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := PlanCursorHooks(root, false)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = read(t, root, CursorHooksPath)
	if strings.Contains(got, "terma") || !strings.Contains(got, "./format.sh") || !strings.Contains(got, "./audit.sh") || !strings.Contains(got, `"version": 1`) {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

// A file terma creates carries the schema version Cursor requires, and goes away whole
// on uninstall — the version it added is not a reason to keep an otherwise empty file.
func TestCursorHooksCreatedAndRemovedWhole(t *testing.T) {
	root := t.TempDir()
	plan, _ := PlanCursorHooks(root, true)
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, CursorHooksPath)
	if !strings.Contains(got, "terma hook cursor-session-start") || !strings.Contains(got, `"version": 1`) {
		t.Fatalf("file wrong:\n%s", got)
	}
	un, _ := PlanCursorHooks(root, false)
	if len(un.Changes) != 1 || un.Changes[0].Action() != "delete" {
		t.Fatalf("expected a delete, got %+v", un.Changes)
	}
}

func TestCursorHooksRefusesMalformedFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, CursorHooksPath, `{"version": 1, "hooks": [`)
	if _, err := PlanCursorHooks(root, true); err == nil {
		t.Fatal("a file terma cannot parse must not be rewritten")
	}
}

func TestHasCursor(t *testing.T) {
	root := t.TempDir()
	if HasCursor(root) {
		t.Fatal("no .cursor yet")
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !HasCursor(root) {
		t.Fatal("a .cursor directory means the repository is used with Cursor")
	}
}

// Capture continues through follow-up loops without changing anyone else's limit.
func TestCursorObservationHooksPreserveUserPolicy(t *testing.T) {
	root := t.TempDir()
	write(t, root, CursorHooksPath, `{"version":1,"hooks":{"stop":[{"command":"./continue.sh","loop_limit":2,"timeout":42,"future_option":true}],"beforeSubmitPrompt":[{"command":"./policy.sh","failClosed":true}]}}`)
	p, err := PlanCursorHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = Apply(root, p); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err = json.Unmarshal([]byte(read(t, root, CursorHooksPath)), &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc.Hooks["stop"][0]["loop_limit"]) != "2" || string(doc.Hooks["stop"][1]["loop_limit"]) != "null" {
		t.Fatal(doc.Hooks["stop"])
	}
	// subagentStop has the same five-loop default; only stop-shaped hooks get the null.
	if string(doc.Hooks["subagentStop"][0]["loop_limit"]) != "null" || doc.Hooks["afterAgentResponse"][0]["loop_limit"] != nil {
		t.Fatal(doc.Hooks["subagentStop"], doc.Hooks["afterAgentResponse"])
	}
	if _, ok := doc.Hooks["subagentStart"]; ok {
		t.Fatal("subagentStart is a permission gate: never committed")
	}
	for _, gate := range []string{"preToolUse", "beforeShellExecution", "beforeMCPExecution", "beforeReadFile"} {
		if _, ok := doc.Hooks[gate]; ok {
			t.Fatalf("%s is a permission gate: never committed", gate)
		}
	}
	for _, dup := range []string{"afterShellExecution", "afterMCPExecution"} {
		if _, ok := doc.Hooks[dup]; ok {
			t.Fatalf("%s restates postToolUse without a call id: never committed", dup)
		}
	}
	if string(doc.Hooks["beforeSubmitPrompt"][0]["failClosed"]) != "true" {
		t.Fatal("user policy changed")
	}
	for _, name := range []string{"beforeSubmitPrompt", "afterAgentResponse", "preCompact", "stop", "subagentStop", "postToolUse", "postToolUseFailure"} {
		if len(doc.Hooks[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	p, err = PlanCursorHooks(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = Apply(root, p); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, CursorHooksPath)
	if strings.Contains(got, "terma") || !strings.Contains(got, "future_option") || !strings.Contains(got, "failClosed") {
		t.Fatal(got)
	}
}
