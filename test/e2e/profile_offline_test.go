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
