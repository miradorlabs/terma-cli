package live

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Validate every matching record, not just the first arrival. Each check can
// also run on malformed offline evidence without launching a harness.
type telemetryEvidence struct {
	logs     []LogRecord
	spans    []Span
	metrics  []Metric
	requests []ExportRequest
}

func (r *Receiver) evidence() telemetryEvidence {
	return telemetryEvidence{r.Logs(), r.Spans(), r.Metrics(), r.Requests()}
}

func requireFields(t contractReporter, label string, attrs map[string]string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if strings.TrimSpace(attrs[key]) == "" {
			t.Errorf("%s: missing/empty %s", label, key)
		}
	}
}
func equalField(t contractReporter, label string, attrs map[string]string, key, want string) {
	t.Helper()
	if attrs[key] != want {
		t.Errorf("%s.%s = %q, want %q", label, key, attrs[key], want)
	}
}

func (e telemetryEvidence) logsFor(t contractReporter, event, sessionKey, sid string, min int, fields ...string) []LogRecord {
	t.Helper()
	var out []LogRecord
	for _, r := range e.logs {
		if r.Attrs["event.name"] != event || r.Attrs[sessionKey] != sid {
			continue
		}
		out = append(out, r)
		requireFields(t, event, r.Attrs, fields...)
		requireFields(t, event, r.Resource, "service.name")
		if sessionKey == "session.id" && r.Time.IsZero() {
			t.Errorf("%s: missing OTLP timestamp", event)
		}
		if _, err := time.Parse(time.RFC3339Nano, r.Attrs["event.timestamp"]); err != nil {
			t.Errorf("%s: invalid event.timestamp", event)
		}
	}
	if len(out) < min {
		t.Errorf("%s: got %d records for %s, need %d", event, len(out), sid, min)
	}
	return out
}

func checkExportRequests(t contractReporter, e telemetryEvidence, paths ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, r := range e.requests {
		seen[r.Path] = true
		if r.Authorization != "Bearer "+liveKey {
			t.Errorf("%s: export used wrong/missing authorization", r.Path)
		}
		if r.DecodeError != nil {
			t.Errorf("%s: invalid OTLP: %v", r.Path, r.DecodeError)
		}
	}
	if len(paths) == 0 {
		paths = []string{"/v1/logs", "/v1/traces", "/v1/metrics"}
	}
	for _, p := range paths {
		if !seen[p] {
			t.Errorf("no export to %s", p)
		}
	}
}

func checkSpans(t contractReporter, e telemetryEvidence, sessionKey, sid, project string, names ...string) map[string][]Span {
	t.Helper()
	found := map[string][]Span{}
	ids := map[string]Span{}
	for _, s := range e.spans {
		if s.Attrs[sessionKey] != sid {
			continue
		}
		found[s.Name] = append(found[s.Name], s)
		p := s.Proto
		if p == nil {
			t.Errorf("%s: missing OTLP span", s.Name)
			continue
		}
		if len(p.TraceId) != 16 || bytes.Equal(p.TraceId, make([]byte, 16)) || len(p.SpanId) != 8 || bytes.Equal(p.SpanId, make([]byte, 8)) {
			t.Errorf("%s: invalid trace/span IDs", s.Name)
		}
		key := hex.EncodeToString(p.TraceId) + "/" + hex.EncodeToString(p.SpanId)
		if _, ok := ids[key]; ok {
			t.Errorf("%s: duplicate span ID", s.Name)
		}
		ids[key] = s
		if p.StartTimeUnixNano == 0 || p.EndTimeUnixNano < p.StartTimeUnixNano {
			t.Errorf("%s: invalid span timing", s.Name)
		}
		if p.GetStatus().GetCode() == 2 {
			t.Errorf("%s: unexpected error status", s.Name)
		}
		requireFields(t, s.Name, s.Resource, "service.name")
		if project != "" && s.Resource["mirador.project.id"] != project && s.Attrs["mirador.project.id"] != project {
			t.Errorf("%s: missing/wrong project identity", s.Name)
		}
	}
	for _, name := range names {
		if len(found[name]) == 0 {
			t.Errorf("missing span %s for %s", name, sid)
		}
	}
	// Every non-root span in our single isolated turn must join an exported parent.
	for _, s := range ids {
		p := s.Proto
		if len(p.ParentSpanId) > 0 && !bytes.Equal(p.ParentSpanId, make([]byte, 8)) {
			if _, ok := ids[hex.EncodeToString(p.TraceId)+"/"+hex.EncodeToString(p.ParentSpanId)]; !ok {
				t.Errorf("%s: parent span was not exported in this session", s.Name)
			}
		}
	}
	return found
}

func checkClaudeTelemetry(t contractReporter, e telemetryEvidence, sid, project string, exclude bool) {
	t.Helper()
	checkExportRequests(t, e)
	checkHookLifecycle(t, e, sid, project)
	if exclude {
		checkRedaction(t, e, "claude")
	}
	common := []string{"prompt.id", "event.sequence", "user.id"}
	prompts := e.logsFor(t, "user_prompt", "session.id", sid, 1, append(common, "prompt", "prompt_length", "message.uuid")...)
	promptID := ""
	for _, r := range prompts {
		equalField(t, "user_prompt", r.Attrs, "prompt", map[bool]string{false: telemetryPrompt, true: "<REDACTED>"}[exclude])
		equalField(t, "user_prompt", r.Attrs, "prompt_length", fmt.Sprint(len(telemetryPrompt)))
		promptID = r.Attrs["prompt.id"]
	}
	requests := e.logsFor(t, "api_request", "session.id", sid, 2, append(common, "request_id", "model", "duration_ms", "ttft_ms", "speed", "cost_usd", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens")...)
	checkClaudeRequests(t, requests)
	requestIDs := map[string]bool{}
	for _, r := range requests {
		requestIDs[r.Attrs["request_id"]] = true
		for k, v := range map[string]string{"input_tokens": "12", "output_tokens": "4", "cache_read_tokens": "3", "cache_creation_tokens": "2", "prompt.id": promptID, "model": "claude-haiku-4-5"} {
			equalField(t, "api_request", r.Attrs, k, v)
		}
		duration := checkNumber(t, "api_request.duration_ms", r.Attrs["duration_ms"], 0, math.Inf(1), false)
		checkNumber(t, "api_request.ttft_ms", r.Attrs["ttft_ms"], 0, duration, false)
	}
	for _, r := range e.logsFor(t, "assistant_response", "session.id", sid, 1, append(common, "response", "response_length", "request_id", "model", "message.uuid")...) {
		equalField(t, "assistant_response", r.Attrs, "response", map[bool]string{false: "TERMA_TELEMETRY_REPLY", true: "<REDACTED>"}[exclude])
		equalField(t, "assistant_response", r.Attrs, "response_length", "21")
		equalField(t, "assistant_response", r.Attrs, "prompt.id", promptID)
		if !requestIDs[r.Attrs["request_id"]] {
			t.Errorf("assistant_response: request_id does not join api_request")
		}
	}
	decisions := e.logsFor(t, "tool_decision", "session.id", sid, 1, append(common, "tool_name", "tool_use_id", "decision", "source")...)
	for _, r := range decisions {
		for k, v := range map[string]string{"tool_name": "Bash", "tool_use_id": "toolu_telemetry", "decision": "accept", "source": "config", "prompt.id": promptID} {
			equalField(t, "tool_decision", r.Attrs, k, v)
		}
	}
	for _, r := range e.logsFor(t, "tool_result", "session.id", sid, 1, append(common, "tool_name", "tool_use_id", "success", "duration_ms", "tool_input_size_bytes", "tool_result_size_bytes")...) {
		for k, v := range map[string]string{"tool_name": "Bash", "tool_use_id": "toolu_telemetry", "success": "true", "prompt.id": promptID} {
			equalField(t, "tool_result", r.Attrs, k, v)
		}
		if !exclude && !strings.Contains(r.Attrs["tool_input"], telemetryCommand) {
			t.Errorf("tool_result: lost tool arguments")
		}
		for _, k := range []string{"duration_ms", "tool_input_size_bytes", "tool_result_size_bytes"} {
			checkNumber(t, "tool_result."+k, r.Attrs[k], 0, math.Inf(1), true)
		}
	}
	// Claude is routed by its project key; it does not stamp Terma resource attributes.
	spans := checkSpans(t, e, "session.id", sid, "", "claude_code.interaction", "claude_code.llm_request", "claude_code.tool", "claude_code.tool.execution", "claude_code.tool.blocked_on_user")
	checkClaudeLogTraceJoins(t, e, sid)
	for _, s := range spans["claude_code.llm_request"] {
		requireFields(t, s.Name, s.Attrs, "model", "request_id", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "duration_ms", "ttft_ms", "stop_reason", "success")
		if !requestIDs[s.Attrs["request_id"]] {
			t.Errorf("llm_request span does not join api_request")
		}
		equalField(t, s.Name, s.Attrs, "input_tokens", "12")
		equalField(t, s.Name, s.Attrs, "output_tokens", "4")
	}
	output := false
	for _, s := range append(spans["claude_code.tool.execution"], spans["claude_code.tool"]...) {
		equalField(t, s.Name, s.Attrs, "tool_use_id", "toolu_telemetry")
		for _, event := range s.Proto.GetEvents() {
			if event.Name == "tool.output" && strings.Contains(fmt.Sprint(flatten(event.Attributes)), "TERMA_TELEMETRY_TOOL") {
				output = true
			}
		}
	}
	if !exclude && !output {
		t.Errorf("tool.execution: missing tool.output content")
	}
	// Exact usage totals also catch dropped second requests and cache double counting.
	for kind, want := range map[string]float64{"input": 24, "output": 8, "cacheRead": 6, "cacheCreation": 4} {
		checkSumMetric(t, e, "claude_code.token.usage", map[string]string{"session.id": sid, "type": kind}, want, "")
	}
	checkSumMetric(t, e, "claude_code.session.count", map[string]string{"session.id": sid}, 1, "")
	checkSumMetric(t, e, "claude_code.cost.usage", map[string]string{"session.id": sid}, SpendOf(requests), "")
	checkSumMetric(t, e, "claude_code.active_time.total", map[string]string{"session.id": sid}, -1, "")
}

func attrsMatch(attrs, want map[string]string) bool {
	for k, v := range want {
		if attrs[k] != v {
			return false
		}
	}
	return true
}

func checkSumMetric(t contractReporter, e telemetryEvidence, name string, filter map[string]string, want float64, project string) {
	t.Helper()
	total := 0.0
	count := 0
	// Claude exports delta sums by default. Do not silently sum cumulative snapshots.
	for _, m := range e.metrics {
		if m.Proto.GetName() != name {
			continue
		}
		sum := m.Proto.GetSum()
		if sum == nil || sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA || !sum.IsMonotonic {
			t.Errorf("%s: expected monotonic delta sum", name)
			continue
		}
		if project != "" {
			equalField(t, name, m.Resource, "mirador.project.id", project)
		}
		for _, p := range sum.DataPoints {
			if !attrsMatch(flatten(p.Attributes), filter) {
				continue
			}
			count++
			if p.StartTimeUnixNano == 0 || p.TimeUnixNano < p.StartTimeUnixNano {
				t.Errorf("%s: invalid metric timestamp", name)
			}
			var v float64
			switch n := p.Value.(type) {
			case *metricspb.NumberDataPoint_AsInt:
				v = float64(n.AsInt)
			case *metricspb.NumberDataPoint_AsDouble:
				v = n.AsDouble
			default:
				t.Errorf("%s: missing numeric value", name)
			}
			total += checkNumber(t, name, v, 0, math.Inf(1), false)
		}
	}
	if count == 0 {
		t.Errorf("missing metric %s %v", name, filter)
	}
	if want < 0 {
		checkPositive(t, name, total, false)
	} else if math.Abs(total-want) > 1e-9 {
		t.Errorf("%s %v = %g, want %g", name, filter, total, want)
	}
}

// Wait for the entire contract, never just the first matching log. The harness
// has exited; this also waits for detached Terma hook delivery.
func awaitTelemetry(t *testing.T, sb *Sandbox, check func(contractReporter, telemetryEvidence)) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		evidence := sb.Receiver.evidence()
		var failures contractFailures
		check(&failures, evidence)
		if len(failures) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, event := range sb.Spool() {
				t.Logf("still queued: %s for %s", event.Name, event.SessionID)
			}
			t.Logf("captured SessionEnd hooks: codex=%d claude=%d", len(sb.HookPayloads("codex-session-end")), len(sb.HookPayloads("session-end")))
			for _, span := range evidence.spans {
				if strings.Contains(span.Name, "hook") || strings.Contains(span.Name, "shutdown") {
					t.Logf("lifecycle span %s: start=%d end=%d attrs=%v events=%v", span.Name, span.Proto.GetStartTimeUnixNano(), span.Proto.GetEndTimeUnixNano(), span.Attrs, span.Proto.GetEvents())
				}
			}
			for _, failure := range slices.Compact(failures) {
				t.Error(failure)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func checkCodexTelemetry(t contractReporter, e telemetryEvidence, sid, project string, exclude bool) {
	t.Helper()
	checkExportRequests(t, e)
	checkHookLifecycle(t, e, sid, project)
	if exclude {
		checkRedaction(t, e, "codex")
	}
	if !exclude {
		found := false
		for _, r := range e.logs {
			if r.Attrs["event.name"] == "terma.assistant.message" && r.Attrs["session.id"] == sid && r.Attrs["text"] == "TERMA_TELEMETRY_REPLY" {
				found = true
				requireFields(t, "terma.assistant.message", r.Attrs, "message_id", "turn_id", "evidence_source")
				equalField(t, "terma.assistant.message", r.Resource, "mirador.project.id", project)
			}
		}
		if !found {
			t.Errorf("Codex reply was not delivered by the rollout hook")
		}
	}
	common := []string{"model", "app.version", "auth_mode", "originator"}
	e.logsFor(t, "codex.conversation_starts", "conversation.id", sid, 1, append(common, "approval_policy", "sandbox_policy", "provider_name", "reasoning_effort")...)
	for _, r := range e.logsFor(t, "codex.user_prompt", "conversation.id", sid, 1, append(common, "prompt", "prompt_length")...) {
		equalField(t, "codex.user_prompt", r.Attrs, "prompt", map[bool]string{false: telemetryPrompt, true: "[REDACTED]"}[exclude])
		equalField(t, "codex.user_prompt", r.Attrs, "prompt_length", fmt.Sprint(len(telemetryPrompt)))
	}
	for _, r := range e.logsFor(t, "codex.api_request", "conversation.id", sid, 2, append(common, "attempt", "http.response.status_code", "duration_ms")...) {
		equalField(t, "codex.api_request", r.Attrs, "http.response.status_code", "200")
		checkNumber(t, "codex.api_request.duration_ms", r.Attrs["duration_ms"], 0, math.Inf(1), false)
	}
	completed := []LogRecord{}
	for _, r := range e.logsFor(t, "codex.sse_event", "conversation.id", sid, 1, append(common, "event.kind")...) {
		// A stream timing record and a usage record share response.completed.
		// Require both shapes; do not mistake a timing-only record for zero usage.
		if r.Attrs["event.kind"] != "response.completed" {
			continue
		}
		if _, timing := r.Attrs["duration_ms"]; timing {
			checkNumber(t, "sse.duration_ms", r.Attrs["duration_ms"], 0, math.Inf(1), false)
			continue
		}
		completed = append(completed, r)
		for k, v := range map[string]string{"input_token_count": "12", "output_token_count": "4", "cached_token_count": "3", "reasoning_token_count": "1", "cache_write_token_count": "0"} {
			equalField(t, "response.completed", r.Attrs, k, v)
		}
		checkNumber(t, "response.completed.tool_token_count", r.Attrs["tool_token_count"], 0, math.Inf(1), true)
	}
	if len(completed) != 2 {
		t.Errorf("response.completed: got %d usage records, want 2", len(completed))
	}
	checkCodexCompleted(t, completed)
	for _, r := range e.logsFor(t, "codex.tool_decision", "conversation.id", sid, 1, append(common, "call_id", "tool_name", "decision", "source")...) {
		equalField(t, "codex.tool_decision", r.Attrs, "call_id", "call_telemetry")
		equalField(t, "codex.tool_decision", r.Attrs, "decision", "approved")
		equalField(t, "codex.tool_decision", r.Attrs, "source", "Config")
	}
	for _, r := range e.logsFor(t, "codex.tool_result", "conversation.id", sid, 1, append(common, "call_id", "tool_name", "success", "duration_ms", "arguments")...) {
		equalField(t, "codex.tool_result", r.Attrs, "call_id", "call_telemetry")
		equalField(t, "codex.tool_result", r.Attrs, "success", "true")
		for key, marker := range map[string]string{"arguments": telemetryCommand, "output": "TERMA_TELEMETRY_TOOL"} {
			if key == "output" && exclude {
				if strings.Contains(r.Attrs[key], marker) {
					t.Errorf("codex.tool_result: excluded output leaked")
				}
				continue
			}
			if !strings.Contains(r.Attrs[key], marker) {
				t.Errorf("codex.tool_result: lost %s", key)
			}
		}
		checkNumber(t, "codex.tool_result.duration_ms", r.Attrs["duration_ms"], 0, math.Inf(1), false)
	}
	// Codex puts the session on the turn span; child spans inherit its trace,
	// not its attributes. Validate the graph using the native IDs.
	var turn *Span
	for i := range e.spans {
		s := &e.spans[i]
		if s.Name == "session_task.turn" && s.Attrs["thread.id"] == sid {
			turn = s
		}
	}
	if turn == nil {
		t.Errorf("missing session_task.turn for %s", sid)
	} else {
		requireFields(t, turn.Name, turn.Attrs, "turn.id", "model")
		for k, v := range map[string]string{"input_tokens": "24", "output_tokens": "8", "cached_input_tokens": "6", "reasoning_output_tokens": "2", "total_tokens": "32"} {
			equalField(t, turn.Name, turn.Attrs, "codex.turn.token_usage."+k, v)
		}
		trace := telemetryEvidence{}
		for _, s := range e.spans {
			if bytes.Equal(s.Proto.GetTraceId(), turn.Proto.GetTraceId()) {
				trace.spans = append(trace.spans, s)
			}
		}
		checkSpans(t, trace, "", "", project, "session_task.turn", "handle_responses")
		usageSpans, tools := 0, 0
		for _, s := range trace.spans {
			if _, ok := s.Attrs["gen_ai.usage.input_tokens"]; ok {
				usageSpans++
				equalField(t, s.Name, s.Attrs, "gen_ai.usage.input_tokens", "12")
				equalField(t, s.Name, s.Attrs, "gen_ai.usage.output_tokens", "4")
				equalField(t, s.Name, s.Attrs, "gen_ai.usage.cache_read.input_tokens", "3")
			}
			if s.Attrs["call_id"] == "call_telemetry" {
				tools++
			}
		}
		if usageSpans != 2 {
			t.Errorf("got %d request usage spans, want 2", usageSpans)
		}
		if tools == 0 {
			t.Errorf("no tool span joins call_telemetry")
		}
	}
	// Codex metrics intentionally have no session/project dimensions. Each
	// sandbox has its own receiver, so counts cannot be satisfied by another run.
	for _, name := range []string{"codex.api_request.duration_ms", "codex.sse_event.duration_ms", "codex.tool.call.duration_ms", "codex.turn.e2e_duration_ms", "codex.turn.ttft.duration_ms"} {
		checkCodexHistogram(t, e, name, nil, -1)
	}
	checkCodexHistogram(t, e, "codex.turn.tool.call", nil, 1)
	for kind, want := range map[string]float64{"input": 24, "output": 8, "cached_input": 6, "reasoning_output": 2, "total": 32} {
		checkCodexHistogram(t, e, "codex.turn.token_usage", map[string]string{"token_type": kind}, want)
	}
	for name, want := range map[string]float64{"codex.api_request": 2, "codex.tool.call": 1} {
		checkSumMetric(t, e, name, nil, want, "")
	}
}

func checkCodexHistogram(t contractReporter, e telemetryEvidence, name string, filter map[string]string, want float64) {
	t.Helper()
	var count uint64
	var total float64
	for _, m := range e.metrics {
		if m.Proto.GetName() != name {
			continue
		}
		h := m.Proto.GetHistogram()
		if h == nil || h.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
			t.Errorf("%s: expected delta histogram", name)
			continue
		}
		requireFields(t, name, m.Resource, "service.name")
		for _, p := range h.DataPoints {
			a := flatten(p.Attributes)
			if !attrsMatch(a, filter) {
				continue
			}
			requireFields(t, name, a, "auth_mode", "originator", "session_source", "model", "app.version")
			if p.StartTimeUnixNano == 0 || p.TimeUnixNano < p.StartTimeUnixNano {
				t.Errorf("%s: invalid metric timestamp", name)
			}
			if p.Sum == nil || p.Count == 0 {
				t.Errorf("%s: missing histogram sum/count", name)
			}
			total += checkNumber(t, name, p.GetSum(), 0, math.Inf(1), false)
			count += p.Count
			buckets := uint64(0)
			for _, n := range p.BucketCounts {
				buckets += n
			}
			if buckets != p.Count || len(p.BucketCounts) != len(p.ExplicitBounds)+1 || !slices.IsSorted(p.ExplicitBounds) {
				t.Errorf("%s: invalid histogram buckets", name)
			}
		}
	}
	if count == 0 {
		t.Errorf("missing histogram %s %v", name, filter)
	}
	if want >= 0 && math.Abs(total-want) > 1e-9 {
		t.Errorf("%s %v sum = %g, want %g", name, filter, total, want)
	}
}

func checkHookLifecycle(t contractReporter, e telemetryEvidence, sid, project string) {
	for _, name := range []string{"terma.session.start", "terma.session.end"} {
		found := false
		for _, r := range e.logs {
			if r.Resource["service.name"] == "terma-cli" && r.Attrs["event.name"] == name && r.Attrs["session.id"] == sid {
				found = true
				equalField(t, name, r.Resource, "mirador.project.id", project)
				requireFields(t, name, r.Attrs, "tool", "project_id")
			}
		}
		if !found {
			t.Errorf("hook never delivered %s for %s", name, sid)
		}
	}
}

func checkRedaction(t contractReporter, e telemetryEvidence, harness string) {
	for _, request := range e.requests {
		if request.DecodeError != nil || request.Payload == nil {
			continue
		}
		data, err := protojson.Marshal(request.Payload)
		if err != nil {
			t.Errorf("encode captured export: %v", err)
			continue
		}
		markers := []string{telemetryPrompt, "TERMA_TELEMETRY_REPLY"}
		// Codex always exports tool arguments; its exclusion flag controls output.
		if harness == "claude" {
			markers = append(markers, "TERMA_TELEMETRY_TOOL")
		}
		for _, marker := range markers {
			if bytes.Contains(data, []byte(marker)) {
				t.Errorf("%s: excluded content leaked through %s", harness, request.Path)
			}
		}
	}
}

func checkClaudeLogTraceJoins(t contractReporter, e telemetryEvidence, sid string) {
	t.Helper()
	ids := map[string]bool{}
	for _, s := range e.spans {
		if s.Attrs["session.id"] == sid {
			ids[hex.EncodeToString(s.Proto.GetTraceId())+"/"+hex.EncodeToString(s.Proto.GetSpanId())] = true
		}
	}
	sequences := map[string]bool{}
	for _, r := range e.logs {
		if r.Attrs["session.id"] != sid || !slices.Contains([]string{"user_prompt", "assistant_response", "api_request", "tool_decision", "tool_result"}, r.Attrs["event.name"]) {
			continue
		}
		checkNumber(t, r.Attrs["event.name"]+".event.sequence", r.Attrs["event.sequence"], 0, math.Inf(1), true)
		seq := r.Attrs["event.sequence"]
		if sequences[seq] {
			t.Errorf("duplicate event.sequence %s", seq)
		}
		sequences[seq] = true
		if !ids[hex.EncodeToString(r.Proto.GetTraceId())+"/"+hex.EncodeToString(r.Proto.GetSpanId())] {
			t.Errorf("%s: native trace/span IDs do not join exported spans", r.Attrs["event.name"])
		}
	}
}
