package hookmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexFile is the shape terma writes and Codex parses: matcher groups, each holding
// command handlers.
type codexFile struct {
	Description string `json:"description"`
	Hooks       map[string][]struct {
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
			Async   bool   `json:"async"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func parseCodex(t *testing.T, raw string) codexFile {
	t.Helper()
	var doc codexFile
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	return doc
}

func TestCodexHooksMergeKeepsDescriptionAndUserGroups(t *testing.T) {
	root := t.TempDir()
	write(t, root, CodexHooksPath, `{
  "description": "team hooks",
  "hooks": {
    "PostToolUse": [{"matcher": "^shell$", "hooks": [{"type": "command", "command": "./audit.sh"}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "./log.sh"}]}]
  }
}
`)
	plan, err := PlanCodexHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	doc := parseCodex(t, read(t, root, CodexHooksPath))

	if doc.Description != "team hooks" {
		t.Fatalf("description lost: %q", doc.Description)
	}
	post := doc.Hooks["PostToolUse"]
	if len(post) != 2 {
		t.Fatalf("want the user's group plus terma's, got %d", len(post))
	}
	if post[0].Matcher == nil || *post[0].Matcher != "^shell$" || post[0].Hooks[0].Command != "./audit.sh" {
		t.Fatalf("user's PostToolUse group changed: %+v", post[0])
	}
	// terma's group carries no matcher: which tool calls touched a file is decided in
	// the binary, not by a regex frozen into a committed file.
	if post[1].Matcher != nil {
		t.Fatalf("terma's group should carry no matcher, got %q", *post[1].Matcher)
	}
	ours := post[1].Hooks[0]
	if ours.Command != HookCommand("codex-post-tool-use") || ours.Type != "command" {
		t.Fatalf("terma's PostToolUse handler wrong: %+v", ours)
	}
	// PostToolUse fires on every tool call, so it must not make the agent wait.
	if !ours.Async {
		t.Fatal("PostToolUse must be async")
	}
	if len(doc.Hooks["UserPromptSubmit"]) != 1 {
		t.Fatalf("unrelated event touched: %+v", doc.Hooks["UserPromptSubmit"])
	}
	start := doc.Hooks["SessionStart"]
	if len(start) != 1 || start[0].Hooks[0].Command != HookCommand("codex-session-start") {
		t.Fatalf("SessionStart missing: %+v", start)
	}
	if start[0].Hooks[0].Async {
		t.Fatal("SessionStart should be synchronous: the session must exist before the first edit")
	}
	stop := doc.Hooks["Stop"]
	if len(stop) != 1 || stop[0].Hooks[0].Command != HookCommand("codex-stop") {
		t.Fatalf("Stop missing: %+v", stop)
	}
	if stop[0].Hooks[0].Async || stop[0].Hooks[0].Timeout != 3 {
		t.Fatal("Stop must finish bounded local capture before codex exec exits")
	}
	end := doc.Hooks["SessionEnd"]
	if len(end) != 1 || end[0].Hooks[0].Command != HookCommand("codex-session-end") {
		t.Fatalf("SessionEnd missing: %+v", end)
	}
	// Codex caps SessionEnd at 3 seconds and defaults to 1; ask for the maximum.
	if end[0].Hooks[0].Timeout != 3 {
		t.Fatalf("SessionEnd timeout = %d, want Codex's maximum of 3", end[0].Hooks[0].Timeout)
	}
	sub := doc.Hooks["SubagentStart"]
	if len(sub) != 1 || sub[0].Hooks[0].Command != HookCommand("codex-subagent-start") || !sub[0].Hooks[0].Async {
		t.Fatalf("SubagentStart should be wired and async: %+v", sub)
	}
	sub = doc.Hooks["SubagentStop"]
	if len(sub) != 1 || sub[0].Hooks[0].Command != HookCommand("codex-subagent-stop") || sub[0].Hooks[0].Async || sub[0].Hooks[0].Timeout != 3 {
		t.Fatalf("SubagentStop should be synchronous and short: %+v", sub)
	}

	if again, _ := PlanCodexHooks(root, true); !again.Empty() {
		t.Fatal("install should be idempotent")
	}

	un, _ := PlanCodexHooks(root, false)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	after := parseCodex(t, read(t, root, CodexHooksPath))
	if after.Description != "team hooks" {
		t.Fatalf("uninstall lost the description: %q", after.Description)
	}
	if len(after.Hooks["PostToolUse"]) != 1 || after.Hooks["PostToolUse"][0].Hooks[0].Command != "./audit.sh" {
		t.Fatalf("uninstall did not restore PostToolUse: %+v", after.Hooks["PostToolUse"])
	}
	if _, ok := after.Hooks["SessionStart"]; ok {
		t.Fatalf("uninstall left SessionStart behind: %+v", after.Hooks)
	}
}

func TestCodexHooksCreateAndRemoveWholeFile(t *testing.T) {
	root := t.TempDir()
	plan, err := PlanCodexHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	doc := parseCodex(t, read(t, root, CodexHooksPath))
	if len(doc.Hooks) != len(CodexHooks) {
		t.Fatalf("want %d events, got %d", len(CodexHooks), len(doc.Hooks))
	}
	// Codex parses this file with unknown fields denied, so a key it does not know
	// would make it reject the whole thing.
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(read(t, root, CodexHooksPath)), &top); err != nil {
		t.Fatal(err)
	}
	for key := range top {
		if key != "hooks" && key != "description" {
			t.Fatalf("unknown top-level key %q would make Codex refuse the file", key)
		}
	}

	un, _ := PlanCodexHooks(root, false)
	if err := Apply(root, un); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(CodexHooksPath))); !os.IsNotExist(err) {
		t.Fatal("a file that held nothing but terma's hooks should be removed")
	}
}

func TestCodexHooksRefuseMalformedFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, CodexHooksPath, "{not json")
	if _, err := PlanCodexHooks(root, true); err == nil {
		t.Fatal("want an error for a file this CLI cannot parse")
	} else if !strings.Contains(err.Error(), CodexHooksPath) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestHasCodex(t *testing.T) {
	root := t.TempDir()
	if HasCodex(root) {
		t.Fatal("no .codex directory")
	}
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !HasCodex(root) {
		t.Fatal("want true once .codex exists")
	}
}

// The key is how Codex names an entry in its trust records, after the hooks file's
// path — read off a developer's own ~/.codex/config.toml: `…/hooks.json:subagent_start:0:0`.
func TestCodexEntryKeyIsCodexsOwn(t *testing.T) {
	for entry, want := range map[CodexEntry]string{
		{Event: "SessionStart"}:                        "session_start:0:0",
		{Event: "PostToolUse", Group: 1}:               "post_tool_use:1:0",
		{Event: "SubagentStart", Group: 0, Handler: 2}: "subagent_start:0:2",
		{Event: "Stop"}:                                "stop:0:0",
	} {
		if got := entry.Key(); got != want {
			t.Errorf("%+v: %q, want %q", entry, got, want)
		}
	}
}

// Only terma's entries are listed, wherever a developer's own sit beside them.
func TestCodexTermaEntriesFindsTermasAmongOthers(t *testing.T) {
	root := t.TempDir()
	if entries, err := CodexTermaEntries(root); err != nil || entries != nil {
		t.Fatalf("a missing file: %v, %v", entries, err)
	}
	write(t, root, CodexHooksPath, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-notifier"}]}]}}`)
	plan, err := PlanCodexHooks(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	entries, err := CodexTermaEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(CodexHooks) {
		t.Fatalf("entries = %d, want one per hook terma installs (%d): %+v", len(entries), len(CodexHooks), entries)
	}
	for _, e := range entries {
		// The developer's own Stop group came first, so terma's is the second group.
		if want := map[bool]int{true: 1, false: 0}[e.Event == "Stop"]; e.Group != want || e.Handler != 0 {
			t.Errorf("%s at %d:%d, want %d:0", e.Event, e.Group, e.Handler, want)
		}
	}
}
