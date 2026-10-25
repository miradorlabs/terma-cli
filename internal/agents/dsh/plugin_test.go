package dsh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The plugin is private (it holds the relay's token), carries its configuration, and is
// inserted into dsh's home patch once, after whatever the developer's patch holds.
func TestWritePluginInsertsItselfOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	patch := filepath.Join(home, "cordis.patch.yml")
	if err := os.WriteFile(patch, []byte("- insert:\n    - id: mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{"/x/terma", "hook"}}
	path, err := writePlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"endpoint":"http://127.0.0.1:43180"`, `"hookCommand":["/x/terma","hook"]`, `"version":1`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("plugin is %v: it holds the relay's token", info.Mode().Perm())
	}
	if _, err := writePlugin(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(patch)
	if !strings.HasPrefix(string(got), "- insert:\n    - id: mine\n") || strings.Count(string(got), "id: terma") != 1 {
		t.Fatalf("patch:\n%s", got)
	}
	if !strings.Contains(dshPluginTemplate, dshConfigMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
	// A refresh leaves the inert template inert.
	if err := os.WriteFile(path, []byte(dshPluginTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
		t.Fatalf("refresh of the raw template: changed=%v err=%v", changed, err)
	}
}

// Removing the plugin takes out only terma's insert, and the patch with it when nothing else was there.
func TestRemovePluginTakesOutItsInsert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	patch := filepath.Join(home, "cordis.patch.yml")
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180"}
	for _, mine := range []string{"- insert:\n    - id: mine\n", ""} {
		if err := os.Remove(patch); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if mine != "" {
			if err := os.WriteFile(patch, []byte(mine), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		path, err := writePlugin(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := removePlugin(); err != nil || len(changed) != 2 {
			t.Fatalf("removePlugin = %v, %v", changed, err)
		}
		got, err := os.ReadFile(patch)
		if mine == "" && !os.IsNotExist(err) || mine != "" && string(got) != mine {
			t.Fatalf("patch after removal: %q, %v", got, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plugin survived: %v", err)
		}
	}
}

// An export goes straight to the local relay whatever proxy dsh's own fetch would route it
// through; the driver stands in for that fetch.
func TestPluginBypassesProxies(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	proxy, proxied := recorder(t)
	relay, received := recorder(t)
	text, err := renderPlugin(pluginConfig{Endpoint: relay.URL, Headers: map[string]string{"Authorization": "Bearer tok"},
		HookCommand: []string{node, "-e", "process.stdin.resume()"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plugin := filepath.Join(dir, "terma-relay.mjs")
	if err := os.WriteFile(plugin, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	const driver = `import { Agent, request } from "node:http"
const [plugin, relay, proxy] = process.argv.slice(2)
// sendViaProxy stands in for a fetch behind a proxy dispatcher: it sends every request to
// the proxy, in the absolute form a proxy takes, on an agent no NODE_USE_ENV_PROXY reaches.
const sendViaProxy = (url, init = {}) => new Promise((resolve, reject) => {
  const via = new URL(proxy)
  const req = request({ host: via.hostname, port: via.port, path: String(url), method: init.method || "GET", headers: init.headers, agent: new Agent() },
    (res) => { res.resume(); res.on("end", () => resolve(new Response(null, { status: res.statusCode }))) })
  req.on("error", reject)
  req.end(init.body)
})
// dsh's own fetch, behind the dispatcher it installs.
globalThis.fetch = sendViaProxy
// A plain fetch first: it goes to the proxy.
await fetch(relay + "/probe", { method: "POST", body: "{}" }).catch(() => {})
const { apply } = await import(plugin)
const on = {}
apply({ on: (name, fn) => { on[name] = fn } })
on["session/event"]({ id: "s1" }, { type: "user/message", data: { content: "hi" } })
on["session/event"]({ id: "s1" }, { type: "turn/end", data: {} })
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script, plugin, relay.URL, proxy.URL)
	// NODE_USE_ENV_PROXY puts node:http's own default agent behind the proxy too.
	cmd.Env = append(withProxy(proxy.URL), "NODE_USE_ENV_PROXY=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got := proxied(); !slices.Equal(got, []string{"/probe "}) {
		t.Errorf("the proxy saw %q, want only the probe", got)
	}
	if got := received(); !slices.Equal(got, []string{"/v1/logs Bearer tok"}) {
		t.Errorf("the relay saw %q, want the export", got)
	}
}

// recorder answers every request with an empty JSON object and reports each one's path
// and Authorization, in order.
func recorder(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// withProxy is this process's environment with proxy as its only HTTP proxy.
func withProxy(proxy string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !strings.HasSuffix(strings.ToLower(name), "_proxy") {
			env = append(env, kv)
		}
	}
	return append(env, "HTTP_PROXY="+proxy, "http_proxy="+proxy)
}
