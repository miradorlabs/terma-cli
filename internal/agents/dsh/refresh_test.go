package dsh

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDshRefreshMachineUpdatesInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_HOME", dir)
	path := filepath.Join(dir, "plugins", "terma-relay.mjs")

	if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
		t.Fatalf("RefreshMachine(absent) = (%q, %v, %v); want (%q, false, nil)", got, changed, err, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `// Terma old dsh plugin
const CONFIG = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer tok"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/custom/terma","hook"]} /* terma:config */
export function apply() {}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
		t.Fatalf("RefreshMachine(outdated) = (%v, %v); want (true, nil)", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/custom/terma"`, `"http://127.0.0.1:43180"`, `"dsh-file-edit"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("refreshed plugin missing %s", want)
		}
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("RefreshMachine(current) = (%v, %v); want (false, nil)", changed, err)
	}
}

// The plugin, run under Node, spans each tool call from its call to its result, hands an
// edit to dsh-file-edit, and hands no tool call to a reach hook.
func TestDshPluginToolCallsUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	var mu sync.Mutex
	var spanNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name string `json:"name"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if r.URL.Path == "/v1/traces" && json.Unmarshal(body, &req) == nil {
			mu.Lock()
			for _, rs := range req.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						spanNames = append(spanNames, sp.Name)
					}
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nf=$(mktemp "+hooksDir+"/call.XXXXXX)\nprintf '{\"event\":\"%s\",\"payload\":' \"$1\" > $f.part\ncat >> $f.part\necho '}' >> $f.part\nmv $f.part $f.json\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	text, err := renderPlugin(pluginConfig{Endpoint: srv.URL, HookCommand: []string{hook}})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const handlers = {}
mod.apply({ on: (name, fn) => { handlers[name] = fn } })
const sess = { id: "session-dsh-1" }
handlers["agent/created"]({ agent: { sessionId: "session-dsh-1", cwd: "/repoA" } })
handlers["session/event"](sess, { type: "tool/call", time: 1, data: { callId: "1", name: "read", arguments: JSON.stringify({ file_path: "/repoB/secret.go" }) } })
handlers["session/event"](sess, { type: "tool/result", time: 2, data: { message: { source: { callId: "1" }, content: "ok" } } })
handlers["session/event"](sess, { type: "tool/call", time: 3, data: { callId: "2", name: "edit", arguments: { file_path: "/repoA/a.go" } } })
handlers["session/event"](sess, { type: "tool/result", time: 4, data: { message: { source: { callId: "2" }, content: "ok" } } })
await handlers["session/event"](sess, { type: "turn/end", time: 5, data: {} })
await new Promise((r) => setTimeout(r, 200))
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	type call struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	byEvent := map[string][]map[string]any{}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		clear(byEvent)
		files, _ := filepath.Glob(filepath.Join(hooksDir, "*.json"))
		for _, f := range files {
			var c call
			if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &c) == nil {
				byEvent[c.Event] = append(byEvent[c.Event], c.Payload)
			}
		}
		if len(files) >= 2 || time.Now().After(deadline) {
			break
		}
	}
	if edits := byEvent["dsh-file-edit"]; len(edits) != 1 || edits[0]["file"] != "/repoA/a.go" {
		t.Errorf("dsh-file-edit calls = %v, want one for /repoA/a.go", edits)
	}
	if reaches := byEvent["dsh-tool-reach"]; len(reaches) != 0 {
		t.Errorf("dsh-tool-reach calls = %v, want none", reaches)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"execute_tool read", "execute_tool edit"} {
		if !slices.Contains(spanNames, want) {
			t.Errorf("spans = %v, want %q", spanNames, want)
		}
	}
}

func TestDshConfigureRefreshDisconnectConcurrentLockSafety(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_HOME", dir)
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}}
	if _, err := writePlugin(cfg); err != nil {
		t.Fatalf("initial writePlugin: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 3 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := writePlugin(cfg); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, _, err := refreshPlugin(); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := removePlugin(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent dsh write/refresh/remove error: %v", err)
	}
	// Check invariant: patch and plugin file must agree (no dangling insert referencing a missing plugin).
	pluginPath := filepath.Join(dir, "plugins", "terma-relay.mjs")
	patchPath := filepath.Join(dir, "cordis.patch.yml")
	_, pluginErr := os.Stat(pluginPath)
	patchData, patchErr := os.ReadFile(patchPath)
	if os.IsNotExist(pluginErr) && patchErr == nil && strings.Contains(string(patchData), pluginPath) {
		t.Fatalf("dangling cordis.patch.yml insert references deleted plugin %s", pluginPath)
	}
	if _, err := removePlugin(); err != nil {
		t.Fatalf("final removePlugin: %v", err)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("plugin still exists after final removePlugin: err=%v", err)
	}
	if _, err := os.Stat(patchPath); !os.IsNotExist(err) {
		t.Fatalf("patch still exists after final removePlugin: err=%v", err)
	}
}
