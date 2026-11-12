package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provider sees the request CaptureProvider records, whole; credentials are not kept.
func TestCaptureProviderOffline(t *testing.T) {
	b := Binary{Harness: "claude", Version: "9.9.9"}
	var seen []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
	})
	srv := httptest.NewServer(CaptureProvider(t, b, next))
	defer srv.Close()

	body := `{"model":"claude-x","system":[{"type":"text","text":"You are.","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("Anthropic-Beta", "prompt-caching-scope-2026-01-05")
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zw.Write([]byte(`{"instructions":"zipped"}`))
	zw.Close()
	req, _ = http.NewRequest("POST", srv.URL+"/responses", &zipped)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer sk-secret")
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}

	if string(seen) == "" {
		t.Fatal("the provider saw no body")
	}
	p := profileOf(t, b)
	if len(p.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(p.Requests))
	}
	first, second := p.Requests[0], p.Requests[1]
	if first.Path != "/v1/messages" || first.Query != "beta=true" || string(first.Body) != body {
		t.Errorf("first request = %+v", first)
	}
	if got := first.Header["x-api-key"]; len(got) != 1 || got[0] != "[redacted]" {
		t.Errorf("x-api-key kept as %q", got)
	}
	if got := first.Header["anthropic-beta"]; len(got) != 1 || got[0] != "prompt-caching-scope-2026-01-05" {
		t.Errorf("anthropic-beta = %q", got)
	}
	if string(second.Body) != `{"instructions":"zipped"}` {
		t.Errorf("gzip body recorded as %q / %q", second.Body, second.Text)
	}
	if got := second.Header["authorization"]; len(got) != 1 || got[0] != "[redacted]" {
		t.Errorf("authorization kept as %q", got)
	}
	for _, r := range p.Requests {
		data, _ := json.Marshal(r)
		if strings.Contains(string(data), "sk-secret") {
			t.Errorf("a credential reached the profile: %s", data)
		}
	}
}

func TestProfileBodyOffline(t *testing.T) {
	if body, text := profileBody("", []byte("plain")); body != nil || text != "plain" {
		t.Errorf("text body = %q, %q", body, text)
	}
	if _, text := profileBody("", []byte{0xff, 0xfe}); text != "base64://4=" {
		t.Errorf("binary body = %q", text)
	}
	if body, text := profileBody("", nil); body != nil || text != "" {
		t.Errorf("empty body = %q, %q", body, text)
	}
}

// One profile a line, sorted, failure kept; a run with none leaves no file behind.
func TestWriteProfilesOffline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	profilesMu.Lock()
	saved := profiles
	profiles = map[profileID]*Profile{}
	profilesMu.Unlock()
	t.Cleanup(func() { profilesMu.Lock(); profiles = saved; profilesMu.Unlock() })

	os.WriteFile(path, []byte("stale"), 0o644)
	if err := WriteProfiles(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a run with no profiles left an earlier run's")
	}

	profilesMu.Lock()
	profiles[profileID{"codex", "0.170.0", "TestCodexProfile"}] = &Profile{Harness: "codex", Version: "0.170.0", Scenario: "TestCodexProfile", Requests: []ProviderRequest{}}
	profiles[profileID{"claude", "2.1.320", "TestClaudeProfile"}] = &Profile{Harness: "claude", Version: "2.1.320", Scenario: "TestClaudeProfile", Failed: true,
		Requests: []ProviderRequest{{Method: "POST", Path: "/v1/messages", Body: json.RawMessage(`{"model":"m"}`)}},
		Help:     map[string]string{"--help": "Usage: claude"}}
	profilesMu.Unlock()
	if err := WriteProfiles(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 4 {
		t.Errorf("profiles.json has %d lines, want [, two profiles, ]:\n%s", lines, data)
	}
	var rows []Profile
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Harness != "claude" || !rows[0].Failed || rows[1].Harness != "codex" {
		t.Errorf("rows = %+v", rows)
	}
	if rows[0].Help["--help"] != "Usage: claude" || string(rows[0].Requests[0].Body) != `{"model":"m"}` {
		t.Errorf("claude row = %+v", rows[0])
	}
}

// A profile a test records without running a harness's build (a unit test's, of a made-up
// version) is the test's to read, and never reaches profiles.json: the nightly once published a
// Claude Code 9.9.8 and 9.9.9 so.
func TestUnitTestProfilesAreNotWrittenOffline(t *testing.T) {
	dir := t.TempDir()
	profilesMu.Lock()
	saved := profiles
	profiles = map[profileID]*Profile{}
	profilesMu.Unlock()
	t.Cleanup(func() { profilesMu.Lock(); profiles = saved; profilesMu.Unlock() })

	made := Binary{Harness: "claude", Version: "9.9.9"}
	profileOf(t, made).Command = []string{"-p"}
	if got := profileOf(t, made).Command; len(got) != 1 {
		t.Fatalf("the test could not read its profile back: %v", got)
	}
	t.Run("scenario", func(t *testing.T) {
		scenarioOf(t, "codex")
		profileOf(t, Binary{Harness: "codex", Version: "0.170.0"})
	})
	if err := WriteProfiles(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []Profile
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Harness != "codex" {
		t.Errorf("profiles.json = %+v, want the scenario's codex profile alone", rows)
	}
}

// A build's npm package folder, found through its node_modules/.bin link, is its <install>; an
// executable outside node_modules has none.
func TestNpmPackageRootOffline(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "node_modules", "@scope", "agent")
	for _, p := range []string{filepath.Join(pkg, "dist"), filepath.Join(dir, "node_modules", ".bin")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{filepath.Join(pkg, "package.json"): "{}", filepath.Join(pkg, "dist", "cli.js"): "", filepath.Join(dir, "solo"): ""} {
		if err := os.WriteFile(name, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "node_modules", ".bin", "agent")
	if err := os.Symlink("../@scope/agent/dist/cli.js", bin); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(pkg)
	if got := npmPackageRoot(bin); got != want {
		t.Errorf("npmPackageRoot(.bin link) = %q, want %q", got, want)
	}
	if got := npmPackageRoot(filepath.Join(dir, "solo")); got != "" {
		t.Errorf("npmPackageRoot(a lone executable) = %q, want none", got)
	}
}

// A recorded request keeps its query as sent but for a credential's value.
func TestRedactQueryOffline(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"beta=true":                           "beta=true",
		"key=AIzaSyabc&alt=sse":               "key=[redacted]&alt=sse",
		"api_key=x&access_token=y&page=2":     "api_key=[redacted]&access_token=[redacted]&page=2",
		"X-Amz-Signature=abc&X-Amz-Date=2026": "X-Amz-Signature=[redacted]&X-Amz-Date=2026",
		"sig=abc&v=1":                         "sig=[redacted]&v=1",
		"q=sk-abcdefghijklmnopqrstuv":         "q=[redacted]",
		"q=hello%20world&flag":                "q=hello%20world&flag",
		"token=":                              "token=",
		"monkey=banana":                       "monkey=banana",
	} {
		if got := RedactQuery(in); got != want {
			t.Errorf("RedactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
