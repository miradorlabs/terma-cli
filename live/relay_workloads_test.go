package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Workload equivalence. Every workload runs twice against identical fake providers:
// once exporting straight to the receiver (the global export of old, where nothing
// was filtered), once through the relay. What reached upstream must be the same —
// every log event as many times, every span and metric name — and the relay must have
// dropped nothing. Whatever a harness does, common or exotic, if the relay loses a
// record of an opted-in session, or a harness release starts putting telemetry where
// the relay cannot attribute it, this is where it shows.

// telemetryShape counts what upstream received from the agent (not terma's own hook
// delivery): log events by name (Codex's SSE events by kind), spans and metrics by name.
func telemetryShape(e telemetryEvidence) map[string]int {
	out := map[string]int{}
	for _, r := range e.logs {
		if r.Resource["service.name"] == "terma-cli" {
			continue
		}
		name := r.Attrs["event.name"]
		if k := r.Attrs["event.kind"]; k != "" {
			name += "/" + k
		}
		out["log "+name]++
	}
	for _, s := range e.spans {
		out["span "+s.Name]++
	}
	for _, m := range e.metrics {
		out["metric "+m.Proto.GetName()] = 1
	}
	return out
}

// volatile are shapes whose counts vary run to run with no relay involved: startup
// phases and the Codex runtime's own bookkeeping spans. Their presence is compared,
// not their count.
func volatile(key string) bool {
	if sometimes(key) {
		return true
	}
	for _, v := range []string{"log codex.startup_phase", "span receiving", "span append_items", "span persist_rollout_items",
		"span realtime_conversation", "span send_raw_response_items", "span record_conversation_items", "span run_hooks_and_record_inputs",
		"span codex.hooks.command", "log codex.sse_event/"} {
		if strings.HasPrefix(key, v) {
			return true
		}
	}
	return false
}

// sometimes are shapes a harness emits on its own schedule, not the workload's: Claude
// Code's periodic retention sweep of its local transcripts. A run may or may not
// include them, relay or none.
func sometimes(key string) bool {
	return key == "log retention_sweep"
}

// compareShapes fails for anything the direct export delivered that the relayed one
// did not, and notes anything extra.
func compareShapes(t *testing.T, direct, relayed map[string]int) {
	t.Helper()
	keys := make([]string, 0, len(direct))
	for k := range direct {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		got := relayed[k]
		switch {
		case got == 0 && sometimes(k):
			Note(t.Name(), k+": not emitted in the relayed run (the harness's own schedule)")
		case got == 0:
			t.Errorf("%s: the direct export delivered %d, the relay none", k, direct[k])
		case got < direct[k] && !volatile(k):
			t.Errorf("%s: the direct export delivered %d, the relay %d", k, direct[k], got)
		}
	}
	var extra []string
	for k := range relayed {
		if direct[k] == 0 {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		Note(t.Name(), "relayed only: "+strings.Join(extra, ", "))
	}
}

// runBoth runs a workload directly and through the relay, compares, and checks the
// relay dropped nothing.
func runBoth(t *testing.T, sandbox func(t *testing.T) *Sandbox, run func(t *testing.T, sb *Sandbox)) {
	t.Helper()
	var direct map[string]int
	t.Run("direct", func(t *testing.T) {
		sb := sandbox(t)
		run(t, sb)
		time.Sleep(6 * time.Second)
		direct = telemetryShape(sb.Receiver.evidence())
	})
	t.Run("relay", func(t *testing.T) {
		track(t)
		sb := sandbox(t)
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		run(t, sb)
		time.Sleep(6 * time.Second)
		relayed := telemetryShape(sb.Receiver.evidence())
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		if len(direct) == 0 {
			t.Fatal("the direct run delivered nothing to compare with")
		}
		compareShapes(t, direct, relayed)
		if n := sum(c, "dropped."); n > 0 {
			t.Errorf("the relay dropped %d records of an opted-in session: %v", n, c)
		}
	})
}

// --- Claude ------------------------------------------------------------------------

// claudeStep is one response: the tool calls it asks for, all at once (none: a reply).
type claudeStep []map[string]any

// claudeWorkloadProvider answers call n with steps[n-1]'s tool calls, in one message,
// then with a reply. failFirst answers the first call with an overloaded error, which
// Claude retries.
func claudeWorkloadProvider(calls *atomic.Int32, steps []claudeStep, failFirst bool) http.Handler {
	var failed atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		if failFirst && failed.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		call := int(calls.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", fmt.Sprintf("req_wl_%d", call))
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_wl_%d", call), "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 0}}})
		stop := "end_turn"
		if call <= len(steps) && len(steps[call-1]) > 0 {
			stop = "tool_use"
			for i, tool := range steps[call-1] {
				emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_wl_%d_%d", call, i), "name": tool["name"], "input": map[string]any{}}})
				input, _ := json.Marshal(tool["input"])
				emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
				emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			}
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "TERMA_TELEMETRY_REPLY ✓ — مرحبا — 你好"}})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		}
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	})
}

func bash(cmd string) map[string]any {
	return map[string]any{"name": "Bash", "input": map[string]any{"command": cmd, "description": "workload"}}
}

func TestRelayWorkloadsClaude(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		file := func(sb *Sandbox) string { return filepath.Join(sb.Repo, "work.txt") }
		workloads := []struct {
			name      string
			steps     func(sb *Sandbox) []claudeStep
			tools     string
			failFirst bool
			prompt    string
		}{
			{"reply", func(*Sandbox) []claudeStep { return nil }, "", false, "Say hello."},
			{"bash", func(*Sandbox) []claudeStep { return []claudeStep{{bash("printf ok")}} }, "Bash", false, "Run it."},
			{"bash-fails", func(*Sandbox) []claudeStep { return []claudeStep{{bash("echo nope >&2; exit 3")}} }, "Bash", false, "Run it."},
			{"large-output", func(*Sandbox) []claudeStep { return []claudeStep{{bash("head -c 400000 /dev/zero | tr '\\\\0' x")}} }, "Bash", false, "Run it."},
			{"many-tools", func(*Sandbox) []claudeStep {
				var s []claudeStep
				for i := range 8 {
					s = append(s, claudeStep{bash(fmt.Sprintf("printf step-%d", i))})
				}
				return s
			}, "Bash", false, "Run them."},
			{"parallel-tools", func(*Sandbox) []claudeStep {
				return []claudeStep{{bash("printf a"), bash("printf b"), bash("printf c")}}
			}, "Bash", false, "Run them together."},
			{"file-tools", func(sb *Sandbox) []claudeStep {
				return []claudeStep{
					{{"name": "Write", "input": map[string]any{"file_path": file(sb), "content": "one\n"}}},
					{{"name": "Read", "input": map[string]any{"file_path": file(sb)}}},
					{{"name": "Edit", "input": map[string]any{"file_path": file(sb), "old_string": "one", "new_string": "two"}}},
				}
			}, "Write,Read,Edit", false, "Edit the file."},
			{"subagent", func(*Sandbox) []claudeStep {
				return []claudeStep{{{"name": "Agent", "input": map[string]any{"description": "check", "prompt": "Reply.", "subagent_type": "general-purpose"}}}}
			}, "Agent", false, "Use a subagent."},
			{"unicode", func(*Sandbox) []claudeStep { return []claudeStep{{bash("printf '✓ مرحبا 你好 🚀'")}} }, "Bash", false, "Grüße ✓ — مرحبا — 你好 🚀"},
			{"provider-overloaded", func(*Sandbox) []claudeStep { return nil }, "", true, "Say hello."},
		}
		for _, w := range workloads {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, func(t *testing.T) *Sandbox {
					t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
					return New(t, Isolated, WithClaude(b))
				}, func(t *testing.T, sb *Sandbox) {
					var calls atomic.Int32
					provider := httptest.NewServer(claudeWorkloadProvider(&calls, w.steps(sb), w.failFirst))
					defer provider.Close()
					sb.ClaudeBaseURL = provider.URL
					args := []string{"--max-turns", "12", "--permission-mode", "acceptEdits"}
					if w.tools != "" {
						args = append(args, "--tools", w.tools, "--allowedTools", w.tools)
					}
					sb.ClaudeHeadless(RouteAPIKey, w.prompt, args...)
				})
			})
		}
	})
}

// --- Codex -------------------------------------------------------------------------

// codexWorkloadProvider answers call n with a shell call running cmds[n-1], then with
// a reply, naming the shell tool the build exposes.
func codexWorkloadProvider(calls *atomic.Int32, cmds []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		call := calls.Add(1)
		if int(call) > len(cmds) {
			writeCodexResponse(w, call, map[string]any{"type": "message", "id": "msg_wl", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}}})
			return
		}
		cmd := cmds[call-1]
		tool, args := "exec_command", ""
		defs, _ := request["tools"].([]any)
		for _, raw := range defs {
			entry, _ := raw.(map[string]any)
			switch name, _ := entry["name"].(string); name {
			case "exec_command", "shell_command":
				tool = name
			case "shell":
				if tool == "exec_command" {
					tool = "shell"
				}
			}
		}
		if tool == "shell" {
			data, _ := json.Marshal(map[string]any{"command": []string{"/bin/sh", "-c", cmd}})
			args = string(data)
		} else {
			data, _ := json.Marshal(map[string]any{"command": cmd, "cmd": cmd})
			args = string(data)
		}
		writeCodexResponse(w, call, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_wl_%d", call), "call_id": fmt.Sprintf("call_wl_%d", call), "name": tool, "arguments": args})
	})
}

func TestRelayWorkloadsCodex(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		workloads := []struct {
			name string
			cmds []string
		}{
			{"reply", nil},
			{"shell", []string{"printf ok"}},
			{"shell-fails", []string{"echo nope >&2; exit 3"}},
			{"large-output", []string{"head -c 400000 /dev/zero | tr '\\0' x"}},
			{"many-tools", []string{"printf 1", "printf 2", "printf 3", "printf 4", "printf 5", "printf 6"}},
			{"writes-a-file", []string{"printf 'hello ✓\\n' > work.txt && cat work.txt"}},
		}
		for _, w := range workloads {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, func(t *testing.T) *Sandbox {
					t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
					return New(t, Isolated, WithCodex(b))
				}, func(t *testing.T, sb *Sandbox) {
					var calls atomic.Int32
					provider := httptest.NewServer(codexWorkloadProvider(&calls, w.cmds))
					defer provider.Close()
					sb.CodexExec(RouteAPIKey, "Do the task. TERMA_WORKLOAD", fixtureCodexArgs(provider.URL)...)
				})
			})
		}
	})
}

// --- OpenCode ----------------------------------------------------------------------

// openAIToolProvider asks OpenCode's bash tool to run cmd on the first call, then
// replies.
func openAIToolProvider(calls *atomic.Int32, cmd string) http.Handler {
	if cmd == "" {
		return openAIToolCallProvider(calls, "", nil)
	}
	return openAIToolCallProvider(calls, "bash", map[string]any{"command": cmd, "description": "workload"})
}

// openAIToolCallProvider asks for one call of tool with args on the first request
// (none when tool is empty), then replies.
func openAIToolCallProvider(calls *atomic.Int32, tool string, args map[string]any) http.Handler {
	reply := openAIChatProvider(calls)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tool == "" || calls.Load() > 0 || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			reply.ServeHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		arguments, _ := json.Marshal(args)
		emit := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		emit(map[string]any{"id": "chatcmpl_tool", "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_oc_1", "type": "function", "function": map[string]any{"name": tool, "arguments": string(arguments)}}}}}}})
		emit(map[string]any{"id": "chatcmpl_tool", "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

func TestRelayWorkloadsOpenCode(t *testing.T) {
	forEachOpenCode(t, func(t *testing.T, b Binary, _ bool) {
		for _, w := range []struct{ name, cmd string }{{"reply", ""}, {"bash", "printf ok"}} {
			t.Run(w.name, func(t *testing.T) {
				var provider *httptest.Server
				runBoth(t, func(t *testing.T) *Sandbox {
					sb := New(t, Isolated)
					var calls atomic.Int32
					provider = httptest.NewServer(openAIToolProvider(&calls, w.cmd))
					t.Cleanup(provider.Close)
					sb.UseOpenCodeProvider(provider.URL)
					return sb
				}, func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.connectHarness("opencode")
					}
					sb.OpenCodeRun(b, sb.Repo, "", "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}
