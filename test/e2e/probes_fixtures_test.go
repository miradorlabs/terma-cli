package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// The probes' fixture providers (probes.go says what a probe is): the suite's own, made to say
// what a probe needs (the model asked for, a usage, a tool call only where tools are offered).

// claudeProbeProvider is claudeWorkloadProvider for probes: every reply names the model its
// request asked for and reports usage(body) of the request's body (a small usage where usage is
// nil or returns nil), and the nth call offering tools takes steps[n-1]'s tool calls.
func claudeProbeProvider(calls *atomic.Int32, steps []claudeStep, usage func(body []byte) map[string]any) http.Handler {
	var stepped atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/messages/count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"input_tokens":42}`)
			return
		}
		call := int(calls.Add(1))
		var req struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		step := 0
		if len(req.Tools) > 0 {
			step = int(stepped.Add(1))
		}
		u := claudeUsage(12, 4, 0, 0, 0)
		if usage != nil {
			if v := usage(body); v != nil {
				u = v
			}
		}
		output := u["output_tokens"]
		start := map[string]any{}
		for k, v := range u {
			start[k] = v
		}
		start["output_tokens"] = 0
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", fmt.Sprintf("req_probe_%d", call))
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_probe_%d", call), "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": start}})
		stop := "end_turn"
		if step > 0 && step <= len(steps) && len(steps[step-1]) > 0 {
			stop = "tool_use"
			for i, tool := range steps[step-1] {
				emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_probe_%d_%d", call, i), "name": tool["name"], "input": map[string]any{}}})
				input, _ := json.Marshal(tool["input"])
				emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
				emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			}
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "TERMA_TELEMETRY_REPLY"}})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		}
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": output}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	})
}

// openAIProbeProvider is an OpenAI-compatible chat completions endpoint for probes: the first
// request offering tools asks for one call of tool with args (none where tool is empty), and every
// other request, one offering no tools (a session title) or one after the tool's result, gets the
// reply.
func openAIProbeProvider(calls *atomic.Int32, tool string, args map[string]any) http.Handler {
	reply := openAIChatProvider(calls)
	toolCall := openAIToolCallProvider(new(atomic.Int32), tool, args)
	var asked atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		if tool == "" || len(req.Tools) == 0 || !strings.HasSuffix(r.URL.Path, "/chat/completions") || !asked.CompareAndSwap(false, true) {
			reply.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		toolCall.ServeHTTP(w, r)
	})
}

// codexProbeProvider is codexWorkloadProvider for probes: call n runs cmds[n-1] in the shell
// tool the build offers, then every call replies, each reporting usage() (the workload's small
// usage where it is nil or returns nil).
func codexProbeProvider(calls *atomic.Int32, cmds []string, usage func() map[string]any) http.Handler {
	workload := codexWorkloadProvider(new(atomic.Int32), cmds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var u map[string]any
		if usage != nil {
			u = usage()
		}
		if u == nil {
			workload.ServeHTTP(w, r)
			return
		}
		workload.ServeHTTP(&usageWriter{ResponseWriter: w, usage: u}, r)
	})
}
