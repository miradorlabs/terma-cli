package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const telemetryPrompt = "Run the requested tool, then reply exactly TERMA_TELEMETRY_REPLY."
const telemetryCommand = "printf TERMA_TELEMETRY_TOOL"

// These scenarios use real harnesses and exporters with deterministic provider
// responses. No provider credentials, model compliance or paid calls are needed.
//
// Claude runs two ways. content connects it machine-wide (`terma connect`, which sends
// everything straight to Terma). install-content/install-redacted set it up the way
// `terma install` does for a developer: its exporter at the local relay, which forwards
// this repository's sessions under the team's policy, collecting content or not.
func TestClaudeTelemetry(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		for _, tc := range []struct {
			name            string
			exclude, routed bool
		}{
			{"content", false, false},
			{"install-content", false, true},
			{"install-redacted", true, true},
		} {
			exclude := tc.exclude
			t.Run(tc.name, func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				sb.ExcludeContent = exclude
				if tc.routed {
					sb.RouteClaude()
				}
				var calls atomic.Int32
				provider := httptest.NewServer(claudeTelemetryProvider(&calls))
				defer provider.Close()
				sb.ClaudeBaseURL = provider.URL
				_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
				if calls.Load() != 2 {
					t.Errorf("provider calls = %d, want tool request and final reply", calls.Load())
				}
				awaitTelemetry(t, sb, func(reporter contractReporter, e telemetryEvidence) {
					checkClaudeTelemetry(reporter, e, sid, sb.ProjectID, exclude)
				})
				checkTelemetrySchema(t, sb.Receiver.evidence(), "claude", exclude, newest)
			})
		}
	})
}

func TestCodexTelemetry(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		for _, exclude := range []bool{false, true} {
			t.Run(map[bool]string{false: "content", true: "redacted"}[exclude], func(t *testing.T) {
				track(t)
				t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithCodex(b))
				sb.ExcludeContent = exclude
				// Only the relay withholds content, under the team's policy.
				if exclude {
					sb.UseRelay(RelayOptions{Start: true})
				}
				var calls atomic.Int32
				provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
				defer provider.Close()
				run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
				t.Cleanup(func() {
					if t.Failed() {
						t.Logf("Codex stdout:\n%s\nCodex stderr:\n%s", run.Stdout, run.Stderr)
					}
				})
				if calls.Load() != 2 {
					t.Errorf("provider calls = %d, want tool request and final reply", calls.Load())
				}
				awaitTelemetry(t, sb, func(reporter contractReporter, e telemetryEvidence) {
					checkCodexTelemetry(reporter, e, run.ThreadID, sb.ProjectID, exclude, false)
				})
				if knownUpstream(upstreamCodexSessionEnd) && len(sb.Delivered("terma.session.end", run.ThreadID, 10*time.Second)) == 0 {
					t.Logf("KNOWN UPSTREAM: Codex exited without running SessionEnd; tolerated by TERMA_E2E_KNOWN_UPSTREAM (TestCodexSessionEndProbe)")
				}
				checkTelemetrySchema(t, sb.Receiver.evidence(), "codex", exclude, newest)
			})
		}
	})
}

func writeCodexResponse(w http.ResponseWriter, call int32, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(name string, payload map[string]any) {
		payload["type"] = name
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	}
	rid := fmt.Sprintf("resp_telemetry_%d", call)
	emit("response.created", map[string]any{"response": map[string]any{"id": rid, "object": "response", "status": "in_progress", "output": []any{}}})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	emit("response.completed", map[string]any{"response": map[string]any{"id": rid, "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"total_tokens": 16, "input_tokens": 12, "output_tokens": 4, "input_tokens_details": map[string]any{"cached_tokens": 3}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}}}})
}

// claudeTelemetryProvider is a deterministic Anthropic Messages endpoint: the first call
// asks for the Bash tool, the second replies with TERMA_TELEMETRY_REPLY.
func claudeTelemetryProvider(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", fmt.Sprintf("req_telemetry_%d", call))
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_telemetry_%d", call), "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 0, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}}})
		stop := "end_turn"
		if call == 1 {
			stop = "tool_use"
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_telemetry", "name": "Bash", "input": map[string]any{}}})
			input, _ := json.Marshal(map[string]any{"command": telemetryCommand})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "TERMA_TELEMETRY_REPLY"}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	})
}

// codexTelemetryProvider is the same for Codex's Responses endpoint.
func codexTelemetryProvider(t *testing.T, calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		call := calls.Add(1)
		var item map[string]any
		if call == 1 {
			// Select the exposed shell tool; tool naming differs across releases.
			tool := ""
			var args string
			toolDefs, _ := request["tools"].([]any)
			for _, raw := range toolDefs {
				entry, _ := raw.(map[string]any)
				name, _ := entry["name"].(string)
				switch name {
				case "exec_command", "shell_command":
					tool = name
					data, _ := json.Marshal(map[string]any{"command": telemetryCommand, "cmd": telemetryCommand})
					args = string(data)
				case "shell":
					if tool == "" {
						tool = name
						data, _ := json.Marshal(map[string]any{"command": []string{"/bin/sh", "-c", telemetryCommand}})
						args = string(data)
					}
				}
			}
			if tool == "" {
				// Current builds describe tools in the prompt instead of tools[].
				tool = "exec_command"
				args = `{"cmd":"printf TERMA_TELEMETRY_TOOL"}`
			}
			item = map[string]any{"type": "function_call", "id": "fc_telemetry", "call_id": "call_telemetry", "name": tool, "arguments": args}
		} else {
			item = map[string]any{"type": "message", "id": "msg_telemetry", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}}}
		}
		writeCodexResponse(w, call, item)
	})
}
