package pifamily

import (
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
)

// The agent and lifecycle switch reach the rendered extension, the file is private, and
// the raw template stays inert.
func TestWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "extensions", "terma.ts")
	cfg := Config{Agent: "omp", Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{"/x/terma", "hook"}}
	if _, err := Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"agent":"omp"`, `"lifecycle":false`, `"hookCommand":["/x/terma","hook"]`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("extension lacks %s", w)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("extension is %v: it holds the relay's token", info.Mode().Perm())
	}
	if !strings.Contains(template, configMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
	// A refresh leaves the inert template inert.
	raw := filepath.Join(t.TempDir(), "raw.ts")
	if err := os.WriteFile(raw, []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := Refresh(raw); err != nil || changed {
		t.Fatalf("refresh of the raw template: changed=%v err=%v", changed, err)
	}
}

// An export goes straight to the local relay whatever proxy the host would route it
// through: on Bun, the environment's HTTP_PROXY, which its fetch reads itself; on Node, the
// proxy Pi puts its fetch behind, which the driver stands in for. Each runtime found runs
// the extension; CI names them in TERMA_EXPORTER_RUNTIMES, so one missing there fails.
func TestExtensionBypassesProxies(t *testing.T) {
	t.Parallel()
	names, required := []string{"bun", "node"}, false
	if s := os.Getenv("TERMA_EXPORTER_RUNTIMES"); s != "" {
		names, required = strings.Split(s, ","), true
	}
	// A runtime runs the extension if it runs TypeScript: Bun always, Node by default from
	// 22.18 and 23.6, earlier with the flag that turns type stripping on.
	probe := filepath.Join(t.TempDir(), "probe.ts")
	if err := os.WriteFile(probe, []byte("const ok: boolean = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var runtimes [][]string
	for _, name := range names {
		name = strings.TrimSpace(name)
		path, err := exec.LookPath(name)
		var rt []string
		if err == nil {
			for _, flags := range [][]string{nil, {"--experimental-strip-types"}} {
				if err = exec.Command(path, append(flags, probe)...).Run(); err == nil {
					rt = append([]string{path}, flags...)
					break
				}
			}
		}
		switch {
		case rt != nil:
			runtimes = append(runtimes, rt)
		case required:
			t.Fatalf("%s cannot run the extension: %v", name, err)
		}
	}
	if len(runtimes) == 0 {
		t.Skip("neither bun nor a node that runs TypeScript")
	}
	const driver = `const [plugin, relay, proxy] = process.argv.slice(2)
if (!("Bun" in globalThis)) {
  // Pi's own fetch, behind its proxy dispatcher.
  const direct = globalThis.fetch
  globalThis.fetch = (url, init) => direct(proxy + new URL(url).pathname, init)
}
// A plain fetch first: it goes to the proxy, which is live for 127.0.0.1.
await fetch(relay + "/probe", { method: "POST", body: "{}" }).catch(() => {})
const { default: extension } = await import(plugin)
const on = {}
extension({ on: (name, fn) => { on[name] = fn } })
const ctx = { sessionManager: { getSessionId: () => "s1" }, cwd: process.cwd() }
on.before_agent_start({ prompt: "hi" }, ctx)
await on.session_shutdown({}, ctx)
`
	for _, rt := range runtimes {
		t.Run(filepath.Base(rt[0]), func(t *testing.T) {
			t.Parallel()
			proxy, proxied := recorder(t)
			relay, received := recorder(t)
			dir := t.TempDir()
			plugin, err := Write(filepath.Join(dir, "terma.ts"), Config{Agent: "pi", Endpoint: relay.URL,
				Headers: map[string]string{"Authorization": "Bearer tok"}, HookCommand: []string{rt[0], "-e", "process.stdin.resume()"}})
			if err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "driver.mjs")
			if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(rt[0], append(rt[1:], script, plugin, relay.URL, proxy.URL)...)
			// NODE_USE_ENV_PROXY puts node:http's own default agent behind the proxy too.
			cmd.Env = append(withProxy(proxy.URL), "NODE_USE_ENV_PROXY=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", rt[0], err, out)
			}
			// One request, the probe: under NODE_USE_ENV_PROXY a Node may tunnel it with CONNECT.
			if got := proxied(); len(got) != 1 || strings.Contains(got[0], "Bearer") {
				t.Errorf("the proxy saw %q, want only the probe", got)
			}
			if got := received(); !slices.Equal(got, []string{"/v1/logs Bearer tok"}) {
				t.Errorf("the relay saw %q, want the export", got)
			}
		})
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
