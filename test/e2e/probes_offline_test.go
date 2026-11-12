package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// TERMA_E2E_PROBES: all by default, none, or names, a name selecting the probes it prefixes.
func TestProbesSelectedOffline(t *testing.T) {
	for _, c := range []struct {
		spec  string
		names []string
		want  bool
	}{
		{"", []string{"compaction"}, true},
		{"all", []string{"interactive"}, true},
		{"none", []string{"compaction"}, false},
		{"compaction", []string{"compaction"}, true},
		{"compaction", []string{"across-turns"}, false},
		{"retries", []string{"retries-429", "retries-529"}, true},
		{"retries-500", []string{"retries-429", "retries-529"}, false},
		{" tool-output , subagent ", []string{"subagent"}, true},
		{"model-alias", []string{"model-alias-sonnet[1m]"}, true},
		{"model-alias-opus", []string{"model-alias-opus[1m]"}, false},
		{"model", []string{"model-alias-opus"}, true},
		{"price", []string{"prices"}, false},
	} {
		if got := probesSelected(c.spec, c.names...); got != c.want {
			t.Errorf("probesSelected(%q, %q) = %v, want %v", c.spec, c.names, got, c.want)
		}
	}
}

// The first n matching requests fail, with the status, the retry-after and the body; requests
// that do not match, and every one after, reach the provider.
func TestFailingFirstOffline(t *testing.T) {
	var reached atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) })
	match := func(r *http.Request) bool { return r.URL.Path == "/v1/messages" }
	srv := httptest.NewServer(failingFirst(2, 529, 2, `{"type":"error"}`, match, next))
	defer srv.Close()
	post := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("/api/hello"); resp.StatusCode != 200 {
		t.Errorf("a side request was failed: %d", resp.StatusCode)
	}
	for i := range 2 {
		resp := post("/v1/messages")
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 529 || resp.Header.Get("Retry-After") != "2" || string(body) != `{"type":"error"}` {
			t.Errorf("failure %d = %d, retry-after %q, %s", i+1, resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
	}
	if resp := post("/v1/messages"); resp.StatusCode != 200 {
		t.Errorf("the call after the failures = %d", resp.StatusCode)
	}
	if reached.Load() != 2 {
		t.Errorf("the provider saw %d requests, want the side request and the last", reached.Load())
	}
}

// Each request is recorded under the probe set when it arrived, with the fixture's status.
func TestCaptureSwitchOffline(t *testing.T) {
	b := Binary{Harness: "claude", Version: "9.9.8"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(429)
		}
	})
	s := captureSwitch(t, b, "compaction-setup", next)
	srv := httptest.NewServer(s)
	defer srv.Close()
	get := func(path string) {
		t.Helper()
		if _, err := http.Get(srv.URL + path); err != nil {
			t.Fatal(err)
		}
	}
	get("/one")
	s.Set("compaction")
	get("/two")
	get("/fail")
	p := profileOf(t, b)
	var got []string
	for _, r := range p.Requests {
		got = append(got, fmt.Sprintf("%s %s %d", r.Probe, r.Path, r.Status))
	}
	want := []string{"compaction-setup /one 200", "compaction /two 200", "compaction /fail 429"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// claudeEvents is the SSE events of a Messages stream, by name, in order.
func claudeEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.Lines(body) {
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("bad event %q: %v", data, err)
			}
			events = append(events, ev)
		}
	}
	return events
}

// The reply names the model asked for and reports the usage asked for, output tokens in the
// final delta; only a call offering tools takes a tool step.
func TestClaudeProbeProviderOffline(t *testing.T) {
	var calls atomic.Int32
	steps := []claudeStep{{bash("printf ok")}}
	usage := func(body []byte) map[string]any {
		if strings.Contains(string(body), "OUTPUT") {
			return claudeUsage(0, priceTokens, 0, 0, 0)
		}
		return nil
	}
	srv := httptest.NewServer(claudeProbeProvider(&calls, steps, usage))
	defer srv.Close()
	call := func(body string) []map[string]any {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/messages?beta=true", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		return claudeEvents(t, string(data))
	}
	title := call(`{"model":"claude-haiku-x","messages":[]}`)
	if len(title) == 0 || title[0]["message"].(map[string]any)["model"] != "claude-haiku-x" {
		t.Fatalf("the reply does not name the model asked for: %v", title)
	}
	for _, ev := range title {
		if ev["type"] == "content_block_start" && ev["content_block"].(map[string]any)["type"] == "tool_use" {
			t.Errorf("a call offering no tools took the tool step")
		}
	}
	tooled := call(`{"model":"m","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"Say hello. OUTPUT"}]}`)
	var usedTool bool
	var startUsage, deltaUsage map[string]any
	for _, ev := range tooled {
		switch ev["type"] {
		case "message_start":
			startUsage = ev["message"].(map[string]any)["usage"].(map[string]any)
		case "message_delta":
			deltaUsage = ev["usage"].(map[string]any)
		case "content_block_start":
			usedTool = usedTool || ev["content_block"].(map[string]any)["name"] == "Bash"
		}
	}
	if !usedTool {
		t.Error("the call offering tools did not take the tool step")
	}
	if startUsage["output_tokens"] != float64(0) || deltaUsage["output_tokens"] != float64(priceTokens) || startUsage["input_tokens"] != float64(0) {
		t.Errorf("output usage: start %v, delta %v", startUsage, deltaUsage)
	}
	resp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := io.ReadAll(resp.Body); !strings.Contains(string(data), "input_tokens") {
		t.Errorf("count_tokens = %s", data)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want the two message requests", calls.Load())
	}
}

// Only response.completed's usage is replaced; the rest of the stream passes as it was.
func TestCodexProbeProviderUsageOffline(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(codexProbeProvider(&calls, nil, func() map[string]any { return codexUsage(compactionTokens, 0, 4) }))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	var completed, created bool
	for _, ev := range claudeEvents(t, string(data)) {
		switch ev["type"] {
		case "response.created":
			created = true
		case "response.completed":
			completed = true
			usage := ev["response"].(map[string]any)["usage"].(map[string]any)
			if usage["input_tokens"] != float64(compactionTokens) || usage["total_tokens"] != float64(compactionTokens+4) {
				t.Errorf("usage = %v", usage)
			}
		}
	}
	if !created || !completed {
		t.Errorf("stream lost an event:\n%s", data)
	}
}

// The tool call goes to the first request offering tools, not to a title request before it, and
// once only.
func TestOpenAIProbeProviderOffline(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(openAIProbeProvider(&calls, "bash", map[string]any{"command": "printf ok"}))
	defer srv.Close()
	call := func(body string) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}
	if got := call(`{"messages":[{"role":"user","content":"title"}]}`); strings.Contains(got, "tool_calls") {
		t.Errorf("the title request got the tool call: %s", got)
	}
	if got := call(`{"tools":[{"type":"function"}],"messages":[]}`); !strings.Contains(got, `"name":"bash"`) {
		t.Errorf("the request offering tools got no tool call: %s", got)
	}
	if got := call(`{"tools":[{"type":"function"}],"messages":[{"role":"tool"}]}`); strings.Contains(got, "tool_calls") {
		t.Errorf("the request after the tool's result got another call: %s", got)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// The root thread spawns and waits; a sub-agent's request, by its turn metadata, is answered.
func TestCodexSubagentProviderOffline(t *testing.T) {
	var sub atomic.Int32
	srv := httptest.NewServer(codexSubagentProvider(&sub, "TASK"))
	defer srv.Close()
	call := func(agent string) string {
		t.Helper()
		meta, _ := json.Marshal(map[string]string{"agent_name": agent})
		body, _ := json.Marshal(map[string]any{"client_metadata": map[string]string{"x-codex-turn-metadata": string(meta)}})
		resp, err := http.Post(srv.URL+"/responses", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}
	if got := call("/root"); !strings.Contains(got, `"name":"spawn_agent"`) || !strings.Contains(got, "TASK") {
		t.Errorf("first root call = %s", got)
	}
	if got := call("/root/check"); !strings.Contains(got, "TERMA_TELEMETRY_REPLY") {
		t.Errorf("sub-agent call = %s", got)
	}
	if got := call("/root"); !strings.Contains(got, `"name":"wait_agent"`) {
		t.Errorf("second root call = %s", got)
	}
	if got := call("/root"); !strings.Contains(got, "TERMA_TELEMETRY_REPLY") {
		t.Errorf("third root call = %s", got)
	}
	if sub.Load() != 1 {
		t.Errorf("sub-agent calls = %d", sub.Load())
	}
}

// The settings block between `codex exec`'s first two rules, by key.
func TestCodexHeaderOffline(t *testing.T) {
	out := "OpenAI Codex v0.159.1 (research preview)\n--------\nworkdir: /tmp/repo\nmodel: gpt-6.1-sol\nprovider: telemetry_fixture\n" +
		"approval: never\nsandbox: workspace-write [workdir, /tmp, $TMPDIR]\nreasoning effort: none\nreasoning summaries: none\n" +
		"session id: 01a1a4cb-027f-7372-9b2b-8716a8e04650\n--------\nuser\nSay hello: now.\n"
	h := codexHeader(out)
	want := map[string]string{"workdir": "/tmp/repo", "model": "gpt-6.1-sol", "provider": "telemetry_fixture", "approval": "never",
		"sandbox": "workspace-write [workdir, /tmp, $TMPDIR]", "reasoning effort": "none", "reasoning summaries": "none",
		"session id": "01a1a4cb-027f-7372-9b2b-8716a8e04650"}
	if len(h) != len(want) {
		t.Errorf("header = %v", h)
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header[%q] = %q, want %q", k, h[k], v)
		}
	}
	if len(codexHeader("no header here\n")) != 0 {
		t.Error("a header read from output without one")
	}
}

// Each kind's usage, as an api_request record reports it, is read back as that kind, and its
// cost as a price per million tokens.
func TestPriceKindOffline(t *testing.T) {
	for _, k := range priceKinds {
		u := k.usage
		attrs := map[string]string{"input_tokens": fmt.Sprint(u["input_tokens"]), "output_tokens": fmt.Sprint(u["output_tokens"]),
			"cache_read_tokens": fmt.Sprint(u["cache_read_input_tokens"]), "cache_creation_tokens": fmt.Sprint(u["cache_creation_input_tokens"])}
		if kind, tokens, ok := priceKind(attrs); !ok || kind != k.key || tokens != k.tokens {
			t.Errorf("%s read back as %q, %d, %v", k.key, kind, tokens, ok)
		}
	}
	if _, _, ok := priceKind(map[string]string{"input_tokens": "12", "output_tokens": "4"}); ok {
		t.Error("a mixed usage priced")
	}
	if _, _, ok := priceKind(map[string]string{"cache_creation_tokens": "7"}); ok {
		t.Error("a cache write of neither size priced")
	}
	if got := perMillion(0.5, priceTokens); got != 5 {
		t.Errorf("perMillion(0.5, %d) = %v", priceTokens, got)
	}
	if got := perMillion(0.03750000000000001, priceTokens); got != 0.375 {
		t.Errorf("perMillion rounds to %v", got)
	}
}

// What a failed run said: the harness's own message where it printed one.
func TestProbeErrorsOffline(t *testing.T) {
	err := errors.New("claude -p: exit status 1\nmore")
	if got := claudeError(map[string]any{"is_error": true, "result": "API Error: Repeated 529 Overloaded errors"}, err); got != "API Error: Repeated 529 Overloaded errors" {
		t.Errorf("claudeError = %q", got)
	}
	if got := claudeError(nil, err); got != "claude -p: exit status 1" {
		t.Errorf("claudeError without a result = %q", got)
	}
	stdout := `{"type":"thread.started","thread_id":"t"}` + "\n" + `{"type":"error","message":"exceeded retry limit, last status: 429 Too Many Requests"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"exceeded retry limit, last status: 429 Too Many Requests"}}` + "\n"
	if got := codexError(stdout, err); got != "exceeded retry limit, last status: 429 Too Many Requests" {
		t.Errorf("codexError = %q", got)
	}
	if got := codexError("", err); got != "claude -p: exit status 1" {
		t.Errorf("codexError without events = %q", got)
	}
}

// Claude Code's projects/ name of a directory, as a 2.1.285 transcript path showed it.
func TestClaudeProjectDirOffline(t *testing.T) {
	got := claudeProjectDir("/private/var/folders/nc/1hy9_lnp3m/T/TestClaudeProbeToolOutputv2.1.2853311097484/001/repo")
	if want := "-private-var-folders-nc-1hy9-lnp3m-T-TestClaudeProbeToolOutputv2-1-2853311097484-001-repo"; got != want {
		t.Errorf("claudeProjectDir = %q, want %q", got, want)
	}
}

// What toolOutputProbe says the command prints is what it prints.
func TestLargeOutputOffline(t *testing.T) {
	out := largeOutput()
	var want strings.Builder
	for i := 1; i <= toolOutputLines; i++ {
		fmt.Fprintf(&want, toolOutputLine+"\n", i)
	}
	if out.Bytes != want.Len() || out.Lines != toolOutputLines {
		t.Errorf("largeOutput says %d bytes in %d lines, the lines make %d", out.Bytes, out.Lines, want.Len())
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	got, err := exec.Command("sh", "-c", out.Command).Output()
	if err != nil {
		t.Skipf("%s: %v", out.Command, err)
	}
	if string(got) != want.String() {
		t.Errorf("%s printed %d bytes, starting %q", out.Command, len(got), got[:min(len(got), 40)])
	}
}
