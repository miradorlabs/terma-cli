package pi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// piDriver loads the rendered extension under Node with a stand-in for Pi's extension API,
// replays one session's events, and records the URL of every export it sends.
const piDriver = `
const fs = await import("node:fs")
const [ext, fetched] = process.argv.slice(2)
globalThis.fetch = async (url) => { fs.appendFileSync(fetched, String(url) + "\n") }
const handlers = {}
const mod = await import(ext)
mod.default({ on: (name, fn) => { handlers[name] = fn } })
const ctx = { cwd: "/work", model: { id: "m" }, sessionManager: { getSessionId: () => "s1" } }
await handlers.session_start({ reason: "startup" }, ctx)
await handlers.before_agent_start({ prompt: "do it" }, ctx)
handlers.tool_call({ toolCallId: "c1" })
handlers.tool_result({ toolName: "write", toolCallId: "c1", input: { path: "hello.txt", content: "hello" } }, ctx)
handlers.tool_result({ toolName: "read", toolCallId: "c2", input: { path: "notes.txt" } }, ctx)
await handlers.message_end({ message: { role: "assistant", model: "m", usage: { input: 1, output: 1 }, content: [] } }, ctx)
await handlers.session_shutdown({}, ctx)
`

// Pi's extension is its exporter: it sends spans and logs to the relay, and hands Pi's
// hooks the session's start, prompt, end and each file its write and edit change, one
// file per call.
func TestExtensionUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// The extension is TypeScript, which only a Node that strips types loads (22.18+, 23.6+).
	if out, err := exec.Command(node, "-p", "process.features.typescript").Output(); err != nil || !slices.Contains([]string{"strip", "transform"}, strings.TrimSpace(string(out))) {
		t.Skip("node does not run TypeScript")
	}
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	// Each hook call is a process of its own, racing the others: one file per call.
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nf=$(mktemp "+hooks+"/call.XXXXXX)\nprintf '{\"event\":\"%s\",\"payload\":' \"$1\" > $f.part\ncat >> $f.part\necho '}' >> $f.part\nmv $f.part $f.json\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, "terma.ts")
	if _, err := writeExtension(ext, relayExtension(agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "relay-token", HookCommand: []string{hook}})); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(piDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	fetched := filepath.Join(dir, "fetched")
	if out, err := exec.Command(node, script, ext, fetched).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}

	type call struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	byEvent := map[string][]map[string]any{}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		clear(byEvent)
		files, _ := filepath.Glob(filepath.Join(hooks, "*.json"))
		for _, f := range files {
			var c call
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil {
				byEvent[c.Event] = append(byEvent[c.Event], c.Payload)
			}
		}
		if len(files) >= 4 || time.Now().After(deadline) {
			break
		}
	}
	for _, e := range []string{"pi-session-start", "pi-prompt", "pi-file-edit", "pi-session-end"} {
		if len(byEvent[e]) != 1 {
			t.Errorf("%s calls = %v", e, byEvent[e])
		}
	}
	if edits := byEvent["pi-file-edit"]; len(edits) == 1 && (edits[0]["file"] != "hello.txt" || edits[0]["files"] != nil) {
		t.Errorf("edit payload = %v, want file hello.txt", edits[0])
	}
	data, _ := os.ReadFile(fetched)
	for _, path := range []string{"http://127.0.0.1:43180/v1/traces", "http://127.0.0.1:43180/v1/logs"} {
		if !strings.Contains(string(data), path+"\n") {
			t.Errorf("no export to %s; exports:\n%s", path, data)
		}
	}
}
