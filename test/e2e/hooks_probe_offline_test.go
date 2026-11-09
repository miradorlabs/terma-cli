package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// An event's example is its widest payload whole, and its variables' values the first run's,
// each fit to publish: a secret by its name or its shape <redacted>, the PATH <path>, the
// machine's folders and names placeholders, a long text cut. The sandbox's paths stay, for
// normalization.
func TestHooksProbeExampleOffline(t *testing.T) {
	m := machine{home: "/Users/someone", user: "someone", host: "someones-mac"}
	started := []string{"PATH=/opt/homebrew/bin:/usr/bin", "ANTHROPIC_API_KEY=synthetic-telemetry-key", "HOME=/sb/home"}
	env := map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "ANTHROPIC_API_KEY": "synthetic-telemetry-key", "HOME": "/sb/home",
		"CLAUDE_PROJECT_DIR": "/sb/repo", "CLAUDE_CODE_MESSAGING_TOKEN": "tok-0123456789", "CLAUDE_EFFORT": "high",
		"NODE_PATH": "/Users/someone/lib", "CLAUDE_CODE_EXECPATH": "/Users/someone/Work/x/claude", "MANPATH": "/usr/share/man:/opt/homebrew/share/man"}
	long := strings.Repeat("x", hookTextLimit+10)
	calls := []HookCall{
		{Event: "Stop", Variant: hookMatchAll, Env: env, Stdin: `{"hook_event_name":"Stop","cwd":"/sb/repo"}`},
		{Event: "Stop", Variant: hookMatchAll, Env: map[string]string{"CLAUDE_EFFORT": "low"}, Stdin: `{"hook_event_name":"Stop","cwd":"/sb/repo/sub",` +
			`"last_assistant_message":"` + long + `","apiKey":"abc","nested":{"auth_token":"t","list":["/opt/homebrew/bin/node --flag","sk-ant-0123456789abcdefghij"]},` +
			`"note":"tok-0123456789 on someones-mac by someone in /Users/someone/Work/mirador and /private/var/folders/x/T/y","n":3}`},
		{Event: "Setup", Variant: hookMatchAll, Env: env, Stdin: "plain text from /Users/someone"},
	}
	p := hooksProbeOf([]string{"Setup", "Stop"}, "binary", calls, started, newHookScrubber("/sb", started, calls, m))
	stop := p.Events["Stop"]
	var ex map[string]any
	if err := json.Unmarshal(stop.Example, &ex); err != nil {
		t.Fatalf("example = %s: %v", stop.Example, err)
	}
	if ex["cwd"] != "/sb/repo/sub" || ex["apiKey"] != "<redacted>" || ex["n"] != float64(3) {
		t.Errorf("not the widest payload, or not scrubbed: %s", stop.Example)
	}
	nested, _ := ex["nested"].(map[string]any)
	list, _ := nested["list"].([]any)
	if nested["auth_token"] != "<redacted>" || len(list) != 2 || list[0] != "<machine-path> --flag" || list[1] != "<redacted>" {
		t.Errorf("nested = %v", nested)
	}
	if note := ex["note"]; note != "<redacted> on <host> by <user> in <machine-path> and /private/var/folders/x/T/y" {
		t.Errorf("note = %v", note)
	}
	if msg, _ := ex["last_assistant_message"].(string); !strings.HasPrefix(msg, strings.Repeat("x", hookTextLimit)+"…<cut: 10 more characters>") {
		t.Errorf("a long text is not cut: %d characters", len(msg))
	}
	for k, want := range map[string]string{"CLAUDE_PROJECT_DIR": "/sb/repo", "CLAUDE_CODE_MESSAGING_TOKEN": "<redacted>", "CLAUDE_EFFORT": "high",
		"NODE_PATH": "<machine-path>", "CLAUDE_CODE_EXECPATH": "<machine-path>", "MANPATH": "<path>"} {
		if got := stop.EnvValues[k]; got != want {
			t.Errorf("env value %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"PATH", "ANTHROPIC_API_KEY", "HOME"} {
		if _, ok := stop.EnvValues[k]; ok {
			t.Errorf("env value of %s, which the harness was started with", k)
		}
	}
	if string(p.Events["Setup"].Example) != `"plain text from <machine-path>"` {
		t.Errorf("a text payload's example = %s", p.Events["Setup"].Example)
	}
	data, _ := json.Marshal(p)
	for _, leak := range []string{"someone", "homebrew", "synthetic-telemetry-key", "tok-0123456789", "sk-ant"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("the probe holds %q: %s", leak, data)
		}
	}
}

// A name holds a secret by one of its words, in any case style.
func TestSecretNameOffline(t *testing.T) {
	for name, want := range map[string]bool{"ANTHROPIC_API_KEY": true, "apiKey": true, "CLAUDE_CODE_MESSAGING_TOKEN": true, "auth_token": true,
		"x-api-key": true, "GITHUB_TOKEN": true, "client_secret": true, "Authorization": true, "OPENAI_API_KEY": true, "password": true,
		"context_tokens": false, "tool_use_id": false, "CLAUDE_CODE_MESSAGING_SOCKET": false, "keyboard": false, "session_id": false,
		"monkey": false, "transcript_path": false, "CLAUDE_EFFORT": false} {
		if got := secretName(name); got != want {
			t.Errorf("secretName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The probe's MCP server answers the handshake, lists its one tool, and a call of it asks the
// client a question, then returns how it was answered.
func TestElicitingMCPOffline(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- serveElicitingMCP(inR, outW) }()
	dec := json.NewDecoder(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"elicitation":{}}}}`)
	if r := read(); r["id"] != float64(1) || r["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize = %v", r)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r := read(); !strings.Contains(fmt.Sprint(r["result"]), hookMCPTool) {
		t.Errorf("tools/list = %v", r)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{}}}`)
	q := read()
	if q["method"] != "elicitation/create" {
		t.Fatalf("the call asked no question: %v", q)
	}
	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"action":"decline"}}`, q["id"]))
	if r := read(); r["id"] != float64(3) || !strings.Contains(fmt.Sprint(r["result"]), "TERMA_ELICITATION_DECLINE") {
		t.Errorf("tools/call = %v", r)
	}
	_ = inW.Close()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
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
