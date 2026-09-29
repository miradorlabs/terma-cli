package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// claudeScriptedProvider asks for each tool call in steps in turn, one per request,
// then replies with TERMA_TELEMETRY_REPLY.
func claudeScriptedProvider(calls *atomic.Int32, steps []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		call := int(calls.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", fmt.Sprintf("req_scripted_%d", call))
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_scripted_%d", call), "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 0}}})
		stop := "end_turn"
		if call <= len(steps) {
			stop = "tool_use"
			step := steps[call-1]
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_scripted_%d", call), "name": step["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(step["input"])
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
