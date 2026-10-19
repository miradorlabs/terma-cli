package hermes

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

// The spliced configuration must come out of Python exactly as it went in: the token,
// the command, and any character a path or a header can hold.
func TestRenderHermesPluginIsValidPython(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	cfg := pluginConfig{Endpoint: "http://127.0.0.1:43180", Headers: map[string]string{"Authorization": `Bearer t0k"en\é`},
		IncludePrompts: true, HookCommand: []string{"/Users/x y/.local/bin/terma", "hook"}}
	text, err := renderPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-c", "import sys; sys.path.insert(0, sys.argv[1]); import p, json; print(json.dumps(p.CONFIG, sort_keys=True))", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	for _, want := range []string{`"Bearer t0k\"en\\\u00e9"`, `"/Users/x y/.local/bin/terma"`, `"includePrompts": true`, `"includeToolContent": false`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("CONFIG lacks %s:\n%s", want, out)
		}
	}
	// The raw template is inert.
	if err := os.WriteFile(filepath.Join(dir, "raw.py"), []byte(hermesPluginTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(python, "-c", "import sys; sys.path.insert(0, sys.argv[1]); import raw; assert raw.CONFIG is None", dir).CombinedOutput(); err != nil {
		t.Fatalf("the raw template is not inert: %v\n%s", err, out)
	}
}

// An export carries the session's content and the relay's token, so it goes straight to
// the local relay whatever proxy the environment names.
func TestHermesPluginBypassesProxies(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	proxy, proxied := recorder(t)
	relay, received := recorder(t)
	text, err := renderPlugin(pluginConfig{Endpoint: relay.URL, Headers: map[string]string{"Authorization": "Bearer tok"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.py"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	// A plain urllib request first: it goes to the proxy, which is live for 127.0.0.1.
	const script = `import sys, urllib.request
sys.path.insert(0, sys.argv[1])
import p
try:
    urllib.request.urlopen(urllib.request.Request(sys.argv[2] + "/probe", data=b"{}", method="POST"), timeout=10).close()
except Exception:
    pass
p._post("/v1/logs", {"resourceLogs": []})
`
	cmd := exec.Command(python, "-c", script, dir, relay.URL)
	cmd.Env = withProxy(proxy.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
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
