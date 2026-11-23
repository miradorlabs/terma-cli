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

// A "terma" hook holding only the developer's handlers, as teardown leaves one, takes
// terma's entries beside them on the next setup, and loses only those at teardown. One
// that is not an object is refused by setup and left alone by removal.
func TestAntigravityHooksMergeIntoADevelopersTermaHook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{"terma": {"Stop": [{"command": "./my-terma-notes.sh"}]}}`)
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatalf("install over the developer's hook: %v", err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "hooks.json")
	if !strings.Contains(got, "./my-terma-notes.sh") || !strings.Contains(got, testCommand("antigravity-stop")) {
		t.Fatalf("after install:\n%s", got)
	}
	un, err := planUserHooks(root, testCommand, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.ReadFile(t, root, "hooks.json"); strings.Contains(got, "hook --user") || !strings.Contains(got, "./my-terma-notes.sh") {
		t.Fatalf("after removal:\n%s", got)
	}

	other := t.TempDir()
	hookruntest.WriteFile(t, other, "hooks.json", `{"terma": ["not", "a", "hook"]}`)
	if _, err := planUserHooks(other, testCommand, true); err == nil || !strings.Contains(err.Error(), filepath.Join(other, "hooks.json")) {
		t.Errorf("install over a hook that is not an object: %v", err)
	}
	if un, err := planUserHooks(other, testCommand, false); err != nil || !un.Empty() {
		t.Errorf("removal of a hook that is not an object = %+v, %v", un, err)
	}
}

// agy runs none of a hook set "enabled": false: setup switches terma's back on, so a
// planner asked whether anything would change (doctor) says yes until it does.
func TestAntigravityHooksSwitchTermasBackOn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plan, err := planUserHooks(root, testCommand, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(hookruntest.ReadFile(t, root, "hooks.json")), &doc); err != nil {
		t.Fatal(err)
	}
	doc["terma"]["enabled"] = json.RawMessage("false")
	data, _ := json.Marshal(doc)
	hookruntest.WriteFile(t, root, "hooks.json", string(data))

	plan, err = planUserHooks(root, testCommand, true)
	if err != nil || plan.Empty() {
		t.Fatalf("a disabled hook planned as in place: %+v, %v", plan, err)
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "back on") {
		t.Fatalf("setup did not say it switched the hook back on: %q", plan.Notes)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.ReadFile(t, root, "hooks.json"); strings.Contains(got, `"enabled"`) {
		t.Fatalf("setup left terma's hook disabled:\n%s", got)
	}
	if again, _ := planUserHooks(root, testCommand, true); !again.Empty() {
		t.Fatal("install should be idempotent once enabled")
	}
	// A developer's "enabled": true is theirs to keep.
	doc["terma"]["enabled"] = json.RawMessage("true")
	data, _ = json.Marshal(doc)
	hookruntest.WriteFile(t, root, "hooks.json", string(data))
	if again, _ := planUserHooks(root, testCommand, true); !again.Empty() {
		t.Fatal(`setup rewrote "enabled": true`)
	}
}

// A file that is not JSON even as JSONC is refused by an install, naming the file.
// Removal, which setup for any other agent and teardown run, leaves it alone unless it
// holds terma's entries, which it then reports rather than skip.
func TestAntigravityHooksMalformedFile(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"[]", "{", "null", `{"lint": }`} {
		root := t.TempDir()
		hookruntest.WriteFile(t, root, "hooks.json", body)
		if _, err := planUserHooks(root, testCommand, true); err == nil || !strings.Contains(err.Error(), filepath.Join(root, "hooks.json")) {
			t.Errorf("%q: install = %v, want an error naming the file", body, err)
		}
		if plan, err := planUserHooks(root, testCommand, false); err != nil || !plan.Empty() {
			t.Errorf("%q: removal = %+v, %v; want nothing to do", body, plan, err)
		}
	}
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", `{"terma": {"Stop": [{"type": "command", "command": "`+hookruntest.InJSON(testCommand("antigravity-stop"))+`"}]`)
	if _, err := planUserHooks(root, testCommand, false); err == nil {
		t.Error("removal skipped a malformed file holding terma's hooks")
	}
}

// agy reads its hooks file as JSONC: setup and teardown patch one with comments and
// trailing commas in place, keeping the developer's comments.
func TestAntigravityHooksKeepJSONCComments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hookruntest.WriteFile(t, root, "hooks.json", "// my hooks\n{\n  \"lint\": {\"Stop\": [{\"command\": \"./lint.sh\"},]}, // keep lint\n}\n")
	apply := func(install bool) string {
		t.Helper()
		plan, err := planUserHooks(root, testCommand, install)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		return hookruntest.ReadFile(t, root, "hooks.json")
	}
	got := apply(true)
	if !strings.Contains(got, "// my hooks") || !strings.Contains(got, "// keep lint") || !strings.Contains(got, "./lint.sh") || !strings.Contains(got, testCommand("antigravity-stop")) {
		t.Fatalf("after install:\n%s", got)
	}
	if again, err := planUserHooks(root, testCommand, true); err != nil || !again.Empty() {
		t.Fatalf("a second install = %+v, %v", again, err)
	}
	got = apply(false)
	if !strings.Contains(got, "// my hooks") || !strings.Contains(got, "./lint.sh") || strings.Contains(got, "hook --user") || strings.Contains(got, `"terma"`) {
		t.Fatalf("after removal:\n%s", got)
	}
}

// Removal takes out terma's own entries and leaves whatever else is in its hook, of any
// shape: an event that is not a list, an entry that is not an object.
func TestAntigravityHooksRemovalToleratesOddShapes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stop := hookruntest.InJSON(testCommand("antigravity-stop"))
	hookruntest.WriteFile(t, root, "hooks.json", `{"terma": {"PreInvocation": "nonsense", "Stop": [42, {"type": "command", "command": "`+stop+`"}]}}`)
	plan, err := planUserHooks(root, testCommand, false)
	if err != nil {
		t.Fatalf("removal: %v", err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.ReadFile(t, root, "hooks.json")
	if strings.Contains(got, "hook --user") || !strings.Contains(got, `"nonsense"`) || !strings.Contains(got, "42") {
		t.Fatalf("after removal:\n%s", got)
	}
	// Install, which writes the hook, refuses what it cannot merge into.
	if _, err := planUserHooks(root, testCommand, true); err == nil {
		t.Error("install merged into an event that is not a list")
	}
}

// A handler the developer added inside terma's named hook stays through setup and
// teardown; only terma's own entries are replaced or removed.
func TestAntigravityHooksKeepAHandlerAddedToTermas(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	apply := func(cmd func(string) string, install bool) hookmgr.Plan {
		t.Helper()
		plan, err := planUserHooks(root, cmd, install)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(root, plan); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	apply(testCommand, true)
	var doc map[string]map[string][]json.RawMessage
	if err := json.Unmarshal([]byte(hookruntest.ReadFile(t, root, "hooks.json")), &doc); err != nil {
		t.Fatal(err)
	}
	doc["terma"]["Stop"] = append(doc["terma"]["Stop"], json.RawMessage(`{"type":"command","command":"./my-stop.sh","timeout":10}`))
	data, _ := json.Marshal(doc)
	hookruntest.WriteFile(t, root, "hooks.json", string(data))

	if p := apply(testCommand, true); !p.Empty() {
		t.Fatalf("a second setup changed %v", p.Changes)
	}
	moved := hookmgr.UserHookCommand("/usr/local/bin/terma")
	apply(moved, true)
	got := hookruntest.ReadFile(t, root, "hooks.json")
	if !strings.Contains(got, "./my-stop.sh") || !strings.Contains(got, "/usr/local/bin/terma") || strings.Contains(got, "/opt/terma/bin/terma") {
		t.Fatalf("after terma moved:\n%s", got)
	}

	apply(moved, false)
	got = hookruntest.ReadFile(t, root, "hooks.json")
	if strings.Contains(got, "hook --user") || !strings.Contains(got, "./my-stop.sh") {
		t.Fatalf("after teardown:\n%s", got)
	}
	if !strings.Contains(got, `"terma"`) || strings.Contains(got, "PreInvocation") {
		t.Fatalf("teardown should keep the hook with the developer's handler alone:\n%s", got)
	}

	// Setup again after that teardown puts terma's entries back beside the handler.
	apply(moved, true)
	got = hookruntest.ReadFile(t, root, "hooks.json")
	if !strings.Contains(got, "./my-stop.sh") || !strings.Contains(got, moved("antigravity-pre-invocation")) {
		t.Fatalf("after setup again:\n%s", got)
	}
}

// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
func TestAntigravityUserHooksPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := Agent{}.UserHooksPath()
	if err != nil || got != filepath.Join(home, ".gemini", "config", "hooks.json") {
		t.Fatalf("UserHooksPath = %q, %v", got, err)
	}
}
