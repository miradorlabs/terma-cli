package cursor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// testCommand is a machine-wide hook entry's command, as setup writes it.
var testCommand = hookmgr.UserHookCommand("/opt/terma/bin/terma")

func TestCursorHooksMergeKeepsUnknownKeysAndUserHooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{
  "version": 1,
  "hooks": {
    "afterFileEdit": [{"command": "./format.sh"}],
    "beforeShellExecution": [{"command": "./audit.sh"}]
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
	got := hookruntest.ReadFile(t, root, "hooks.json")
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
	if doc.Hooks["afterFileEdit"][1].Command != testCommand("cursor-file-edit") || doc.Hooks["afterFileEdit"][1].Timeout != 10 {
		t.Fatalf("terma hook wrong: %+v", doc.Hooks["afterFileEdit"][1])
	}
	if len(doc.Hooks["beforeShellExecution"]) != 1 {
		t.Fatalf("unrelated hook touched: %+v", doc.Hooks["beforeShellExecution"])
	}
	if len(doc.Hooks["sessionStart"]) != 1 || len(doc.Hooks["sessionEnd"]) != 1 {
		t.Fatalf("session hooks missing: %v", doc.Hooks)
	}
	if again, _ := planUserHooks(root, testCommand, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}
	un, _ := planUserHooks(root, testCommand, false)
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	got = hookruntest.ReadFile(t, root, "hooks.json")
	if strings.Contains(got, "terma") || !strings.Contains(got, "./format.sh") || !strings.Contains(got, "./audit.sh") || !strings.Contains(got, `"version": 1`) {
		t.Fatalf("uninstall wrong:\n%s", got)
	}
}

// The version is not ownership evidence: the same value could have existed before
// install or arrived from a colleague, so uninstall removes commands and keeps it.
func TestCursorUninstallPreservesSchemaVersion(t *testing.T) {
	t.Parallel()
	for _, before := range []string{"", `{"version":1}`, `{"version":42}`} {
		t.Run(before, func(t *testing.T) {
			root := t.TempDir()
			if before != "" {
				hookruntest.WriteFile(t, root, "hooks.json", before)
			}
			plan, err := planUserHooks(root, testCommand, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := hookmgr.Apply(root, plan); err != nil {
				t.Fatal(err)
			}
			un, err := planUserHooks(root, testCommand, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := hookmgr.Apply(root, un); err != nil {
				t.Fatal(err)
			}
			got := hookruntest.ReadFile(t, root, "hooks.json")
			want := before
			if want == "" {
				want = `{"version":1}`
			}
			if !hookmgr.SameJSON([]byte(got), []byte(want)) {
				t.Fatalf("schema changed: %s", got)
			}
		})
	}
}

func TestCursorHooksRefusesMalformedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{"version": 1, "hooks": [`)
	if _, err := planUserHooks(root, testCommand, true); err == nil {
		t.Fatal("a file terma cannot parse must not be rewritten")
	}
}

// Capture continues through follow-up loops without changing anyone else's limit.
func TestCursorObservationHooksPreserveUserPolicy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{"version":1,"hooks":{"stop":[{"command":"./continue.sh","loop_limit":2,"timeout":42,"future_option":true}],"beforeSubmitPrompt":[{"command":"./policy.sh","failClosed":true}]}}`)
	p, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = hookmgr.Apply(root, p); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err = json.Unmarshal([]byte(hookruntest.ReadFile(t, root, "hooks.json")), &doc); err != nil {
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
	p, err = planUserHooks(root, testCommand, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = hookmgr.Apply(root, p); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "hooks.json")
	if strings.Contains(got, "terma") || !strings.Contains(got, "future_option") || !strings.Contains(got, "failClosed") {
		t.Fatal(got)
	}
}
