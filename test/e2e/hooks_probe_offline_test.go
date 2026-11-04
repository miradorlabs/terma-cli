package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// Each harness's hook events are read from its build: Claude Code's settings enum (the longest
// array naming PreToolUse, SessionStart and Stop), Codex's input schemas, Gemini CLI's enum and
// Copilot CLI's HookType schema.
func TestHookEventsInOffline(t *testing.T) {
	claude := []byte(`var a=new Set(["PreToolUse","PostToolUse","SessionStart"]),jm=["PreToolUse","PostToolUse","Notification","SessionStart","Stop","CwdChanged"],` +
		`k=["Stop","PreToolUse","SessionStart"];x=["PreToolUse",b,"SessionStart","Stop","Other"]`)
	if got := ClaudeHookEventsIn(claude); !slices.Equal(got, []string{"CwdChanged", "Notification", "PostToolUse", "PreToolUse", "SessionStart", "Stop"}) {
		t.Errorf("Claude Code = %v", got)
	}
	codex := []byte("pre-tool-use.command.input{\n  \"properties\": {\n    \"hook_event_name\": {\n      \"const\": \"PreToolUse\",\n" +
		"stop.command.input{ \"hook_event_name\": { \"const\": \"Stop\" } } \"hook_event_name\": {\"const\": \"PreToolUse\"}")
	if got := CodexHookEventsIn(codex); !slices.Equal(got, []string{"PreToolUse", "Stop"}) {
		t.Errorf("Codex = %v", got)
	}
	gemini := []byte(`HookEventName2["BeforeTool"] = "BeforeTool"; HookEventName2["SessionStart"] = "SessionStart"; Other["X"] = "X";`)
	if got := GeminiHookEventsIn(gemini); !slices.Equal(got, []string{"BeforeTool", "SessionStart"}) {
		t.Errorf("Gemini CLI = %v", got)
	}
	copilot := []byte(`"HookType": { "type": "string", "enum": [ "preToolUse", "sessionStart" ], "description": "Hook event name.", "title": "HookType" }` +
		`"Other": { "enum": [ "nope" ], "title": "Other" }`)
	if got := CopilotHookEventsIn(copilot); !slices.Equal(got, []string{"preToolUse", "sessionStart"}) {
		t.Errorf("Copilot CLI = %v", got)
	}
}

// The recorder saves each run and answers as its control file says, as many times as it says.
func TestRecordHookOffline(t *testing.T) {
	dir := t.TempDir()
	writeHookControls(t, dir, hookControls{"Stop": {Exit: 2, Stderr: "no", Times: 1}, "SessionStart": {Stdout: "{}"}})
	var out, errOut bytes.Buffer
	if code := recordHook(dir, "Stop", hookMatchAll, nil, strings.NewReader(`{"hook_event_name":"Stop"}`), &out, &errOut); code != 2 || errOut.String() != "no" {
		t.Errorf("first Stop: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := recordHook(dir, "Stop", hookMatchAll, nil, strings.NewReader(`{}`), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Errorf("second Stop: %d %q", code, errOut.String())
	}
	if code := recordHook(dir, "SessionStart", hookMatchNone, []string{"x"}, strings.NewReader(``), &out, &errOut); code != 0 || out.Len() != 0 {
		t.Errorf("the match-nothing hook answered: %d %q", code, out.String())
	}
	calls := HookCallsIn(dir)
	if len(calls) != 3 || calls[0].Event != "Stop" || calls[2].Variant != hookMatchNone || calls[2].Extra[0] != "x" || calls[0].Env["PATH"] == "" {
		t.Errorf("calls = %+v", calls)
	}
}

// A probe's shape: payload key paths four deep with their kinds, the variables the harness
// added or changed (a sandbox path kept, for its placeholder), the first run's directory, and a
// matcher measured by the match-nothing hook.
func TestHooksProbeOfOffline(t *testing.T) {
	root := "/sb"
	started := []string{"HOME=/sb/home", "PATH=/usr/bin", "KEEP=1"}
	env := func(extra ...string) map[string]string {
		m := map[string]string{"HOME": "/sb/home", "PATH": "/sb/bin:/usr/bin", "KEEP": "1", "PWD": "/sb/repo", "__CF_USER_TEXT_ENCODING": "x"}
		for _, kv := range extra {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	calls := []HookCall{
		{Event: "PreToolUse", Variant: hookMatchAll, Cwd: "/sb/repo", Env: env("CLAUDE_PROJECT_DIR=/sb/repo"),
			Stdin: `{"hook_event_name":"PreToolUse","tool_input":{"command":"ls","a":{"b":{"c":{"d":1}}}},"edits":[{"old":"x"}],"n":null}`},
		{Event: "PreToolUse", Variant: hookMatchAll, Cwd: "/sb/repo/sub", Env: env(), Stdin: `{"hook_event_name":"PreToolUse","tool_input":"text"}`},
		{Event: "Stop", Variant: hookMatchAll, Cwd: "/sb/repo", Env: env(), Stdin: `{"hook_event_name":"Stop"}`},
		{Event: "Stop", Variant: hookMatchNone, Cwd: "/sb/repo", Env: env(), Stdin: `{"hook_event_name":"Stop"}`},
		{Event: "Setup", Variant: hookMatchAll, Cwd: "/sb/repo", Env: env(), Stdin: ``},
	}
	p := HooksProbeOf([]string{"Stop", "PreToolUse", "Setup", "Notification"}, "binary", calls, started, root)
	if !slices.Equal(p.Supported, []string{"Notification", "PreToolUse", "Setup", "Stop"}) || p.Source != "binary" {
		t.Errorf("supported = %v (%s)", p.Supported, p.Source)
	}
	pre := p.Events["PreToolUse"]
	if pre == nil || pre.Count != 2 || !*pre.Matcher || pre.Cwd != "/sb/repo" || pre.Stdin != "json" {
		t.Fatalf("PreToolUse = %+v", pre)
	}
	wantKeys := []string{"edits", "edits[].old", "hook_event_name", "n", "tool_input", "tool_input.a", "tool_input.a.b", "tool_input.a.b.c", "tool_input.command"}
	if !slices.Equal(pre.Keys, wantKeys) {
		t.Errorf("keys = %v", pre.Keys)
	}
	if pre.Kinds["tool_input"] != "map|text" || pre.Kinds["n"] != "null" || pre.Kinds["edits"] != "list" || pre.Kinds["tool_input.a.b.c"] != "map" {
		t.Errorf("kinds = %v", pre.Kinds)
	}
	if !slices.Equal(pre.Env, []string{"CLAUDE_PROJECT_DIR", "PATH"}) || pre.EnvPaths["CLAUDE_PROJECT_DIR"] != "/sb/repo" || pre.EnvPaths["PATH"] != "" {
		t.Errorf("env = %v %v", pre.Env, pre.EnvPaths)
	}
	if s := p.Events["Stop"]; s == nil || s.Count != 1 || *s.Matcher {
		t.Errorf("Stop = %+v", s)
	}
	if s := p.Events["Setup"]; s == nil || s.Stdin != "empty" || len(s.Keys) != 0 {
		t.Errorf("Setup = %+v", s)
	}
	notFired(&p, map[string]string{"Notification": "interactive only"})
	if p.NotFired["Notification"] != "interactive only" || len(p.NotFired) != 1 {
		t.Errorf("not fired = %v", p.NotFired)
	}
	data, _ := json.Marshal(p)
	for _, key := range []string{`"supported"`, `"source"`, `"events"`, `"not_fired"`, `"keys"`, `"kinds"`, `"env"`, `"matcher"`, `"cwd"`} {
		if !bytes.Contains(data, []byte(key)) {
			t.Errorf("the probe's JSON has no %s: %s", key, data)
		}
	}
}

// The recording hook's command runs this binary as the recorder, every word quoted.
func TestHookRecorderCommandOffline(t *testing.T) {
	cmd, err := HookRecorderCommand("/tmp/it's here", "PreToolUse", hookMatchAll)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if !strings.HasPrefix(cmd, shellWord(exe)+" "+hookRecorderArg+" ") || !strings.Contains(cmd, `'/tmp/it'\''s here' PreToolUse all`) {
		t.Errorf("command = %s", cmd)
	}
}
