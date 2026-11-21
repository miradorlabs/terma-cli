package pifamily

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// driver loads the rendered extension under Node with a stand-in for omp's extension API,
// replays the events omp 19.0.1 fired for a session that ran a subagent (their shapes as
// a probe extension recorded them), and records any export it attempts.
const driver = `
const [ext, fetched, tmp] = process.argv.slice(2)
globalThis.fetch = async (url) => { require("node:fs").appendFileSync(fetched, String(url) + "\n") }
const handlers = {}
const mod = await import(ext)
mod.default({ on: (name, fn) => { handlers[name] = fn } })
const dir = "/home/dev/.omp/agent/sessions/-work"
const parentFile = dir + "/2026-11-21T13-14-22-769Z_01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf.jsonl"
const sm = (id, file, header) => ({ getSessionId: () => id, getSessionFile: () => file, getHeader: () => header })
const parent = { cwd: "/work", sessionManager: sm("01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf", parentFile, { id: "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf" }) }
const child = { cwd: "/work", sessionManager: sm("01a1fe3e-6e03-7557-b1af-a171883bcfca", parentFile.replace(/\.jsonl$/, "") + "/EchoSub.jsonl", { parentSession: parentFile }) }
const fork = { cwd: "/work", sessionManager: sm("01a1fe3f-0000-7000-8000-000000000003", dir + "/2026-11-21T14-00-00-000Z_01a1fe3f-0000-7000-8000-000000000003.jsonl", { parentSession: parentFile }) }
await handlers.session_start({ reason: "startup" }, parent)
await handlers.before_agent_start({ prompt: "do it" }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "RZPvkTswD|fc_1", isError: true, input: { input: "[notes.txt#B686]", path: "notes.txt", paths: ["notes.txt"] }, details: { diff: "", errorKind: "parse" } }, parent)
handlers.tool_result({ toolName: "write", toolCallId: "6taIXxcqF|fc_2", isError: false, input: { path: "hello.txt", content: "hello" }, details: { resolvedPath: "/work/hello.txt" } }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "h3DEyzO37|fc_3", isError: false, input: { input: "[notes.txt#B686]", path: "notes.txt" }, details: { path: "/work/notes.txt", op: "update" } }, parent)
handlers.tool_result({ toolName: "edit", toolCallId: "multi|fc_4", isError: false, input: {}, details: { perFileResults: [{ path: "/work/a.go" }, { path: "/work/b.go", isError: true }] } }, parent)
handlers.tool_result({ toolName: "read", toolCallId: "paHFu2n2k|fc_5", isError: false, input: { path: "notes.txt" }, details: {} }, parent)
await handlers.session_start({ reason: "startup" }, child)
await handlers.session_start({ reason: "startup" }, fork)
const fs = await import("node:fs")
fs.writeFileSync(tmp + "/Sub.jsonl", '{"type":"title","v":1,"title":""}\n{"type":"session","version":3,"id":"01a1fe3e-6e03-7557-b1af-a171883bcfca"}\n')
const nested = { cwd: "/work", sessionManager: sm("01a1fe40-0000-7000-8000-000000000004", tmp + "/Sub/Inner.jsonl", { parentSession: tmp + "/Sub.jsonl" }) }
await handlers.session_start({ reason: "startup" }, nested)
await handlers.message_end({ message: { role: "assistant", model: "m", usage: { input: 1, output: 1 }, content: [] } }, parent)
await handlers.agent_end?.()
await handlers.session_shutdown({}, parent)
`

// For omp the extension exports nothing and holds no token: it hands omp's hooks each
// session's start, naming a subagent's parent (a nested one's too) but never a fork's, and each file a tool call
// changed, successful ones only, with the call's id as omp's own spans name it.
func TestHooksOnlyExtensionUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
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
	ext := filepath.Join(dir, "terma-omp.ts")
	if _, err := Write(ext, ForHooks("omp", agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "relay-token", HookCommand: []string{hook}})); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(ext); strings.Contains(string(data), "relay-token") {
		t.Fatal("the hooks-only extension holds the relay's token")
	}
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(driver, `require("node:fs")`, `(await import("node:fs"))`)), 0o600); err != nil {
		t.Fatal(err)
	}
	fetched := filepath.Join(dir, "fetched")
	cmd := exec.Command(node, script, ext, fetched, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
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
		if len(calls) >= 9 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	byEvent := map[string][]map[string]any{}
	for _, c := range calls {
		byEvent[c.Event] = append(byEvent[c.Event], c.Payload)
	}
	starts := byEvent["omp-session-start"]
	if len(starts) != 4 {
		t.Fatalf("session starts = %v; calls %v", starts, calls)
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
	edits := map[string]string{}
	for _, e := range byEvent["omp-file-edit"] {
		var files []string
		for _, f := range e["files"].([]any) {
			files = append(files, f.(string))
		}
		edits[e["tool_call_id"].(string)] = strings.Join(files, ",")
	}
	want := map[string]string{"6taIXxcqF|fc_2": "hello.txt", "h3DEyzO37|fc_3": "/work/notes.txt", "multi|fc_4": "/work/a.go"}
	if len(edits) != len(want) {
		t.Errorf("file edits = %v, want %v", edits, want)
	}
	for id, files := range want {
		if edits[id] != files {
			t.Errorf("edit %s named %q, want %q", id, edits[id], files)
		}
	}
	if len(byEvent["omp-prompt"]) != 1 || len(byEvent["omp-session-end"]) != 1 {
		t.Errorf("prompts %v, ends %v", byEvent["omp-prompt"], byEvent["omp-session-end"])
	}
	if data, err := os.ReadFile(fetched); err == nil {
		t.Errorf("the hooks-only extension exported:\n%s", data)
	}
}
