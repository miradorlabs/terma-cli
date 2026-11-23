package omp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// driver loads the rendered extension under Node with a stand-in for OMP's extension API,
// replays the events OMP 19.0.1 fired for a session that ran a subagent (their shapes as
// a probe extension recorded them), and records any export it attempts.
const driver = `
const [ext, fetched, tmp] = process.argv.slice(2)
globalThis.fetch = async (url) => { require("node:fs").appendFileSync(fetched, String(url) + "\n") }
const handlers = {}
const mod = await import(ext)
await mod.default({ on: (name, fn) => { handlers[name] = fn } })
const dir = "/home/dev/.omp/agent/sessions/-work"
const parentFile = dir + "/2026-11-21T13-14-22-769Z_01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf.jsonl"
let title = ""
const sm = (id, file, header) => ({ getSessionId: () => id, getSessionFile: () => file, getHeader: () => header, getSessionName: () => title })
const parent = { cwd: "/work", sessionManager: sm("01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf", parentFile, { id: "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf" }) }
const child = { cwd: "/work", sessionManager: sm("01a1fe3e-6e03-7557-b1af-a171883bcfca", parentFile.replace(/\.jsonl$/, "") + "/EchoSub.jsonl", { parentSession: parentFile }) }
const fork = { cwd: "/work", sessionManager: sm("01a1fe3f-0000-7000-8000-000000000003", dir + "/2026-11-21T14-00-00-000Z_01a1fe3f-0000-7000-8000-000000000003.jsonl", { parentSession: parentFile }) }
await handlers.session_start({ reason: "startup" }, parent)
await handlers.before_agent_start({ prompt: "do it" }, parent)
title = "Fix the flaky build"
await handlers.agent_end({}, parent)
await handlers.before_agent_start({ prompt: "and again" }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "RZPvkTswD|fc_1", isError: true, input: { input: "[notes.txt#B686]", path: "notes.txt", paths: ["notes.txt"] }, details: { diff: "", errorKind: "parse" } }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "6taIXxcqF|fc_2", isError: false, input: { path: "hello.txt", content: "hello" }, details: { resolvedPath: "/work/hello.txt" } }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "h3DEyzO37|fc_3", isError: false, input: { input: "[notes.txt#B686]", path: "notes.txt" }, details: { path: "/work/notes.txt", op: "update" } }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "multi|fc_4", isError: false, input: {}, details: { perFileResults: [{ path: "/work/a.go" }, { path: "/work/b.go", isError: true }] } }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "peer|fc_9", isError: false, input: { path: "agent://scout", content: "hi" }, details: { touchesNoFiles: true } }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "cfg|fc_10", isError: false, input: { path: "cfg://edit.mode", content: "patch" }, details: {} }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "local|fc_11", isError: false, input: { path: "local://notes.md", content: "n" }, details: { resolvedPath: "/home/dev/.omp/local/notes.md" } }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "none|fc_12", isError: false, input: { path: "plain.txt", content: "p" }, details: { resolvedPath: "/work/plain.txt", touchesNoFiles: true } }, parent)
handlers.tool_result({ toolName: "apply_patch", toolCallId: "patch|fc_6", isError: false, input: { input: "*** Begin Patch" }, details: { path: "/work/c.go", op: "update" } }, parent)
handlers.tool_result({ toolName: "apply_patch", toolCallId: "patch|fc_7", isError: false, input: { input: "*** Begin Patch" }, details: { perFileResults: [{ path: "/work/d.go" }, { path: "/work/e.go", isError: true }] } }, parent)
handlers.tool_result({ toolName: "apply_patch", toolCallId: "patch|fc_8", isError: true, input: { input: "*** Begin Patch" }, details: { path: "/work/f.go" } }, parent)
handlers.tool_result({ toolName: "read", toolCallId: "paHFu2n2k|fc_5", isError: false, input: { path: "notes.txt" }, details: {} }, parent)
await handlers.session_start({ reason: "startup" }, child)
await handlers.session_start({ reason: "startup" }, fork)
const fs = await import("node:fs")
fs.writeFileSync(tmp + "/Scout_2.jsonl", '{"type":"title","v":1,"title":""}\n{"type":"session","version":3,"id":"01a1fe3e-6e03-7557-b1af-a171883bcfca"}\n')
const nested = { cwd: "/work", sessionManager: sm("01a1fe40-0000-7000-8000-000000000004", tmp + "/Scout_2/Inner.jsonl", { parentSession: tmp + "/Scout_2.jsonl" }) }
await handlers.session_start({ reason: "startup" }, nested)
await handlers.message_end?.({ message: { role: "assistant", model: "m", usage: { input: 1, output: 1 }, content: [] } }, parent)
await handlers.agent_end?.()
title = "Release notes"
await handlers.session_shutdown({}, parent)
`

// OMP's extension exports nothing, neither spans nor logs, and holds no token: it hands
// OMP's hooks each session's start, naming a subagent's parent (a nested one's too) but
// never a fork's, and each file a tool call changed, successful ones only, with the call's
// id as OMP's own spans name it; an edit under its apply_patch wire name counts as an edit. A
// write names its resolved file, and none when it touches no file (a message to a peer) or
// goes to one of OMP's internal URLs that resolves to none. The session's name goes to
// omp-title once, and again when it changes.
func TestExtensionUnderNode(t *testing.T) {
	byEvent, ext, fetched := runDriver(t)
	data, err := os.ReadFile(ext)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(ext); info.Mode().Perm() != 0o600 {
		t.Errorf("extension is %v, want 0600", info.Mode().Perm())
	}
	for _, w := range []string{relayToken} {
		if strings.Contains(string(data), w) {
			t.Errorf("the extension names %q", w)
		}
	}
	starts := byEvent["omp-session-start"]
	if len(starts) != 4 {
		t.Fatalf("session starts = %v; calls %v", starts, byEvent)
	}
	parents := map[string]any{}
	for _, s := range starts {
		parents[s["session_id"].(string)] = s["parent_session_id"]
	}
	if parents["01a1fe3e-6e03-7557-b1af-a171883bcfca"] != "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf" {
		t.Errorf("the subagent's start named parent %v", parents["01a1fe3e-6e03-7557-b1af-a171883bcfca"])
	}
	if parents["01a1fe40-0000-7000-8000-000000000004"] != "01a1fe3e-6e03-7557-b1af-a171883bcfca" {
		t.Errorf("the nested subagent's start named parent %v, want its opener's id from its header", parents["01a1fe40-0000-7000-8000-000000000004"])
	}
	for _, s := range []string{"01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf", "01a1fe3f-0000-7000-8000-000000000003"} {
		if parents[s] != nil {
			t.Errorf("%s is no subagent, but its start named parent %v", s, parents[s])
		}
	}
	edits, tools := map[string]string{}, map[string]any{}
	for _, e := range byEvent["omp-file-edit"] {
		tools[e["tool_call_id"].(string)] = e["tool"]
		var files []string
		for _, f := range e["files"].([]any) {
			files = append(files, f.(string))
		}
		edits[e["tool_call_id"].(string)] = strings.Join(files, ",")
	}
	want := map[string]string{"6taIXxcqF|fc_2": "/work/hello.txt", "local|fc_11": "/home/dev/.omp/local/notes.md", "h3DEyzO37|fc_3": "/work/notes.txt", "multi|fc_4": "/work/a.go",
		"patch|fc_6": "/work/c.go", "patch|fc_7": "/work/d.go"}
	for _, id := range []string{"patch|fc_6", "patch|fc_7"} {
		if tools[id] != "apply_patch" {
			t.Errorf("patch %s was handed over as tool %v, want apply_patch", id, tools[id])
		}
	}
	if len(edits) != len(want) {
		t.Errorf("file edits = %v, want %v", edits, want)
	}
	for id, files := range want {
		if edits[id] != files {
			t.Errorf("edit %s named %q, want %q", id, edits[id], files)
		}
	}
	var titles []string
	for _, p := range byEvent["omp-title"] {
		if p["session_id"] != "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf" {
			t.Errorf("title for session %v", p["session_id"])
		}
		titles = append(titles, p["title"].(string))
	}
	slices.Sort(titles)
	if strings.Join(titles, "|") != "Fix the flaky build|Release notes" {
		t.Errorf("titles = %v, want the name once and the rename at shutdown", titles)
	}
	if len(byEvent["omp-prompt"]) != 2 || len(byEvent["omp-session-end"]) != 1 {
		t.Errorf("prompts %v, ends %v", byEvent["omp-prompt"], byEvent["omp-session-end"])
	}
	if data, err := os.ReadFile(fetched); err == nil {
		t.Errorf("the hooks-only extension exported:\n%s", data)
	}
}

// runDriver renders the extension for a recording hook command, replays the driver's
// events through it under Node, and returns each hook call's payloads by event, the
// extension's path and the file any export would name.
func runDriver(t *testing.T) (byEvent map[string][]map[string]any, ext, fetched string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// The extension is TypeScript, which only a Node that strips types loads (22.18+, 23.6+).
	if out, err := exec.Command(node, "-p", "process.features.typescript").Output(); err != nil || !slices.Contains([]string{"strip", "transform"}, strings.TrimSpace(string(out))) {
		t.Skip("node does not run TypeScript")
	}
	dir := t.TempDir()
	// Each hook call is a process of its own, racing the others: one file per call.
	hooks := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nf=$(mktemp "+hooks+"/call.XXXXXX)\nprintf '{\"event\":\"%s\",\"payload\":' \"$1\" > $f.part\ncat >> $f.part\necho '}' >> $f.part\nmv $f.part $f.json\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ext = filepath.Join(dir, "terma-ext.ts")
	if _, err := writeExtension(ext, []string{hook}, relayConfig().Endpoint); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(driver, `require("node:fs")`, `(await import("node:fs"))`)), 0o600); err != nil {
		t.Fatal(err)
	}
	fetched = filepath.Join(dir, "fetched")
	if out, err := exec.Command(node, script, ext, fetched, dir).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}

	type call struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	var calls []call
	deadline := time.Now().Add(10 * time.Second)
	for {
		calls = nil
		files, _ := filepath.Glob(filepath.Join(hooks, "*.json"))
		for _, f := range files {
			var c call
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil {
				calls = append(calls, c)
			}
		}
		if len(calls) >= 15 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	byEvent = map[string][]map[string]any{}
	for _, c := range calls {
		byEvent[c.Event] = append(byEvent[c.Event], c.Payload)
	}
	return byEvent, ext, fetched
}

// The extension asks OMP for full content, kept from the tools it runs as its agent .env's
// settings are, only where every signal goes to the relay as OMP resolves it (the shell and
// the launch directory's .env ahead of its own .env files): a collector of the developer's
// for any signal, or none set, and it asks nothing, nor does it in an OMP without the module.
func TestExtensionAsksForContentOnlyForTheRelay(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "-p", "process.features.typescript").Output(); err != nil || !slices.Contains([]string{"strip", "transform"}, strings.TrimSpace(string(out))) {
		t.Skip("node does not run TypeScript")
	}
	withOMP := t.TempDir()
	stub := filepath.Join(withOMP, "node_modules", "@oh-my-pi", "pi-utils")
	if err := os.MkdirAll(stub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"package.json": `{"name":"@oh-my-pi/pi-utils","type":"module","main":"index.js"}`,
		"index.js": `const resolved = JSON.parse(process.env.RESOLVED || "{}")
export function $otelEnv(name) { return resolved[name] }
export function claimOmpOtelEnvFileEntry(name, value) { globalThis.claimed = name + "=" + value; return true }`,
	} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	relay := relayConfig().Endpoint + "/" + relayToken
	signals := func(traces, logs, metrics string) map[string]string {
		return map[string]string{
			signalKey(harness.SignalTraces, "ENDPOINT"):  traces,
			signalKey(harness.SignalLogs, "ENDPOINT"):    logs,
			signalKey(harness.SignalMetrics, "ENDPOINT"): metrics,
		}
	}
	all := signals(relay+"/v1/traces", relay+"/v1/logs", relay+"/v1/metrics")
	const full = ompCaptureContentEnv + "=full"
	cases := map[string]struct {
		dir      string
		resolved map[string]string
		want     string
	}{
		"every signal to the relay":     {withOMP, all, full},
		"the relay's common endpoint":   {withOMP, map[string]string{harness.EnvOTLPEndpoint: relay}, full},
		"traces elsewhere":              {withOMP, signals("https://c.example/v1/traces", relay+"/v1/logs", relay+"/v1/metrics"), "none"},
		"metrics elsewhere":             {withOMP, signals(relay+"/v1/traces", relay+"/v1/logs", "http://127.0.0.1:4318/v1/metrics"), "none"},
		"another port on loopback":      {withOMP, signals("http://127.0.0.1:43181/"+relayToken+"/v1/traces", relay+"/v1/logs", relay+"/v1/metrics"), "none"},
		"only the developer's endpoint": {withOMP, map[string]string{harness.EnvOTLPEndpoint: "https://c.example"}, "none"},
		"nothing set":                   {withOMP, nil, "none"},
		"no OMP module":                 {t.TempDir(), all, "none"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ext := filepath.Join(c.dir, "terma-ext.ts")
			if _, err := writeExtension(ext, []string{"true"}, relayConfig().Endpoint); err != nil {
				t.Fatal(err)
			}
			resolved, err := json.Marshal(c.resolved)
			if err != nil {
				t.Fatal(err)
			}
			script := `const mod = await import(process.argv[1]); await mod.default({ on: () => {} }); console.log((globalThis.claimed ?? "none") + " " + (process.env.` + ompCaptureContentEnv + ` ?? ""))`
			cmd := exec.Command(node, "--input-type=module", "-e", script, ext)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RESOLVED=" + string(resolved)}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, out)
			}
			claimed, live, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
			if claimed != c.want {
				t.Errorf("claimed %s, want %s", claimed, c.want)
			}
			if live != "" {
				t.Errorf("the extension set the process's %s to %q, which the tools OMP runs inherit", ompCaptureContentEnv, live)
			}
		})
	}
}
