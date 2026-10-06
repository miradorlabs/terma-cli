package e2e

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Cursor on a relay machine. cursor-agent's own tracer is fixed to Cursor's backend
// (`${backendUrl}/v1/traces` with Cursor's token, service.name cursor-agent-cli;
// 2026.09.08), so nothing `relay setup` writes redirects it: Cursor never reaches the
// relay, and terma sees it through its hooks alone. Those must work exactly as they do
// without a relay — delivered through the spool, silent — while also claiming the
// conversation, harmlessly: nothing exports under it. Synthetic payloads through the
// installed hook commands and the real binary; Cursor itself needs CURSOR_API_KEY
// (cursor_test.go).
func TestRelayCursorHooks(t *testing.T) {
	track(t)
	cursorHooksUnavailable(t)
	sb := New(t, Isolated)
	sb.UseRelay(RelayOptions{Start: true})
	raw, err := os.ReadFile(filepath.Join(sb.Home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err = json.Unmarshal(raw, &hooks); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"beforeSubmitPrompt", "postToolUse", "afterAgentResponse", "stop"} {
		entries := hooks.Hooks[hook]
		if len(entries) != 1 {
			t.Fatalf("missing installed %s", hook)
		}
		payload, _ := json.Marshal(map[string]any{"conversation_id": "relay-cursor", "generation_id": "turn-a", "workspace_roots": []string{sb.Repo}, "hook_event_name": hook,
			"model": "synthetic-model", "status": "completed", "input_tokens": 10, "output_tokens": 2, "prompt": "private-prompt", "text": "private-response",
			"tool_name": "Shell", "tool_use_id": "call-a", "duration": 5432, "tool_input": map[string]any{"command": "private-command"}, "tool_output": "private-output"})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "sh", "-c", entries[0].Command)
		cmd.Dir = sb.Repo
		cmd.Env = sb.termaEnv()
		cmd.Stdin = strings.NewReader(string(payload))
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || len(out) > 0 {
			t.Fatalf("%s hook changed terminal output on a relay machine: err=%v output=%q", hook, err, out)
		}
	}
	sb.terma(sb.Repo, "spool", "flush", "--quiet")
	if calls := sb.Delivered("terma.tool.call", "relay-cursor", 10*time.Second); len(calls) != 1 {
		t.Fatalf("delivered %d tool calls through the spool, want 1", len(calls))
	}
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "claims", "relay-cursor.json"))
	if err != nil || !strings.Contains(string(data), `"tool":"cursor"`) {
		t.Errorf("the conversation's claim: %s, %v", data, err)
	}
	sb.StopRelay()
	c := sb.RelayStats()
	noteRelayStats(t.Name(), c)
	failUnclassified(t, c)
	if n := sum(c, "received."); n != 0 {
		t.Errorf("the relay received %d records on a Cursor-only machine: %v", n, c)
	}
	for _, r := range sb.Receiver.evidence().logs {
		for _, v := range r.Attrs {
			if strings.Contains(v, "private-") {
				t.Fatalf("Cursor content escaped: %v", r.Attrs)
			}
		}
	}
}

// Claude Code applies settings.json's env — the relay's endpoint and its local token
// in OTEL_EXPORTER_OTLP_HEADERS — to itself, and strips OTEL_* from what its tools run
// (2.1.284). Were a release to pass them on, every program a session runs would export
// to the relay with its token: an OTel-instrumented app under test, or cursor-agent,
// whose bundled OTLP exporter merges OTEL_EXPORTER_OTLP_HEADERS into what it sends to
// Cursor's backend. Nothing would be forwarded (no claimed session names those
// processes), but the token would travel.
func TestRelayClaudeToolsGetNoExporter(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.tools_no_token")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true})
		out := filepath.Join(sb.Dir, "child-env.txt")
		var calls atomic.Int32
		provider := httptest.NewServer(claudeWorkloadProvider(&calls, []claudeStep{{bash("env | cut -d= -f1 | grep '^OTEL_' > " + out + "; true")}}, false))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		sb.ClaudeHeadless(RouteAPIKey, "Run it.", "--max-turns", "4", "--permission-mode", "acceptEdits", "--tools", "Bash", "--allowedTools", "Bash")
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("the tool never ran: %v", err)
		}
		if names := strings.Fields(string(data)); len(names) > 0 {
			t.Errorf("Claude Code passed its exporter settings to a tool's process: %v", names)
		}
	})
}
