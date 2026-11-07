package e2e

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Hook payloads are grouped by the event they name, else the terma hook that ran.
func TestHookShapesOffline(t *testing.T) {
	dir := t.TempDir()
	for name, payload := range map[string]string{
		"1730000000N-session-start.json":  `{"hook_event_name":"SessionStart","session_id":"s","source":"startup"}`,
		"1730000001N-stop.json":           `{"hook_event_name":"Stop","effort":{"level":"high"},"session_id":"s"}`,
		"1730000002N-stop.json":           `{"hook_event_name":"Stop","stop_hook_active":false}`,
		"1730000003N-pi-prompt.json":      `{"prompt":"hi"}`,
		"1730000004N-session-end.json":    ``,
		"1730000005N-gemini-prompt.json":  `not json`,
		"1730000006N-user-prompt.json":    `{"hook_event_name":"UserPromptSubmit","tool_input":[{"a":1}]}`,
		"1730000007N-session-start2.json": `{"hook_event_name":"SessionStart","model":"m"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := HookShapes(dir)
	if s := got["Stop"]; s == nil || s.Count != 2 || !slices.Equal(s.Keys, []string{"effort", "effort.level", "hook_event_name", "session_id", "stop_hook_active"}) {
		t.Errorf("Stop = %+v", s)
	}
	if s := got["SessionStart"]; s == nil || s.Count != 2 || !slices.Equal(s.Keys, []string{"hook_event_name", "model", "session_id", "source"}) {
		t.Errorf("SessionStart = %+v", s)
	}
	if s := got["pi-prompt"]; s == nil || !slices.Equal(s.Keys, []string{"prompt"}) {
		t.Errorf("pi-prompt = %+v", s)
	}
	if s := got["UserPromptSubmit"]; s == nil || !slices.Equal(s.Keys, []string{"hook_event_name", "tool_input", "tool_input[].a"}) {
		t.Errorf("UserPromptSubmit = %+v", s)
	}
	if len(got) != 4 {
		t.Errorf("events = %v", got)
	}
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func nanos(t time.Time) uint64 { return uint64(t.UnixNano()) }

// The telemetry probe reads metric kinds and span parents, names surfaces as the census does,
// and measures each signal's period from what arrived before the turn's last answer.
func TestTelemetryShapeOffline(t *testing.T) {
	start := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "claude-code")}}
	logs := func(at time.Time, records ...time.Time) otlpArrival {
		var lrs []*logspb.LogRecord
		for _, r := range records {
			lrs = append(lrs, &logspb.LogRecord{TimeUnixNano: nanos(r)})
		}
		return otlpArrival{Signal: "logs", At: at, ContentType: "application/x-protobuf", Payload: &collogspb.ExportLogsServiceRequest{
			ResourceLogs: []*logspb.ResourceLogs{{Resource: res, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: lrs}}}}}}
	}
	sum := &metricspb.Metric{Name: "claude_code.token.usage", Unit: "tokens", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
		DataPoints: []*metricspb.NumberDataPoint{{StartTimeUnixNano: nanos(start), TimeUnixNano: nanos(start.Add(60200 * time.Millisecond))}}}}}
	gauge := &metricspb.Metric{Name: "g", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{}}}
	hist := &metricspb.Metric{Name: "h", Unit: "ms", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE}}}
	metrics := otlpArrival{Signal: "metrics", At: start.Add(60300 * time.Millisecond), ContentType: "application/x-protobuf",
		Payload: &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: res,
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{sum, gauge, hist}}}}}}}
	root := &tracepb.Span{Name: "claude_code.interaction", SpanId: []byte("root0001"), EndTimeUnixNano: nanos(start.Add(70 * time.Second))}
	child := &tracepb.Span{Name: "chat claude-x", SpanId: []byte("child001"), ParentSpanId: []byte("root0001"),
		Attributes: []*commonpb.KeyValue{kv("gen_ai.operation.name", "chat")}, EndTimeUnixNano: nanos(start.Add(70 * time.Second))}
	tool := &tracepb.Span{Name: "event core/src/tools/parallel.rs:434", SpanId: []byte("tool0001"), ParentSpanId: []byte("child001")}
	traces := otlpArrival{Signal: "traces", At: start.Add(75 * time.Second), ContentType: "application/x-protobuf",
		Payload: &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: res,
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{root, child, tool}}}}}}}
	terma := otlpArrival{Signal: "logs", At: start.Add(time.Second), ContentType: "application/json", Payload: &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "terma-cli")}},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: nanos(start)}}}}}}}}
	arrivals := []otlpArrival{
		terma,
		logs(start.Add(5100*time.Millisecond), start.Add(100*time.Millisecond)),
		logs(start.Add(12*time.Second), start.Add(7*time.Second), start.Add(9*time.Second)),
		metrics,
		// The exit's flush, after the last answer: no period.
		logs(start.Add(71*time.Second), start.Add(70*time.Second)),
		traces,
	}
	shape := TelemetryShapeOf(arrivals, start.Add(70*time.Second))
	if s := shape.Surfaces["metrics/claude_code.token.usage"]; s == nil || s.Type != "sum" || s.Unit != "tokens" || s.Temporality != "delta" || s.Monotonic == nil || !*s.Monotonic {
		t.Errorf("sum = %+v", s)
	}
	if s := shape.Surfaces["metrics/g"]; s == nil || s.Type != "gauge" || s.Temporality != "" {
		t.Errorf("gauge = %+v", s)
	}
	if s := shape.Surfaces["metrics/h"]; s == nil || s.Type != "histogram" || s.Temporality != "cumulative" {
		t.Errorf("histogram = %+v", s)
	}
	if s := shape.Surfaces["traces/claude_code.interaction"]; s == nil || !s.Root || s.Parent != "" {
		t.Errorf("root span = %+v", s)
	}
	if s := shape.Surfaces["traces/chat {target}"]; s == nil || s.Parent != "traces/claude_code.interaction" {
		t.Errorf("child span = %+v", s)
	}
	if s := shape.Surfaces["traces/event core/src/tools/parallel.rs"]; s == nil || s.Parent != "traces/chat {target}" {
		t.Errorf("span named by its line = %+v", s)
	}
	if got := shape.Export.IntervalMS; got["logs"] != 5000 || got["metrics"] != 60000 || got["traces"] != 0 {
		t.Errorf("intervals = %v", got)
	}
	if got := shape.Export.IntervalBatches; got["logs"] != 2 || got["metrics"] != 1 || len(got) != 2 {
		t.Errorf("interval batches = %v", got)
	}
	if shape.Export.Protocol != "http/protobuf" || shape.Export.Protocols != nil {
		t.Errorf("protocol = %q %v (terma's JSON is not the build's)", shape.Export.Protocol, shape.Export.Protocols)
	}
}

func TestRoundIntervalOffline(t *testing.T) {
	for ms, want := range map[int64]int64{5012: 5000, 4700: 5000, 60450: 60000, 9800: 10000, 1100: 1000, 980: 1000, 13000: 15000, 0: 0} {
		if got := roundInterval(time.Duration(ms) * time.Millisecond); got != want {
			t.Errorf("roundInterval(%dms) = %d, want %d", ms, got, want)
		}
	}
}

// The timing receiver hands every export to the sandbox's receiver unchanged, and notes it.
func TestTimedReceiverOffline(t *testing.T) {
	r := StartReceiver(t)
	tr := TimeReceiver(t, r)
	body, _ := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "api_request"}}}}}}})
	resp, err := http.Post(tr.URL+"/v1/logs", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(r.Logs()) != 1 || r.Logs()[0].EventName != "api_request" {
		t.Errorf("the receiver got %d: %+v", resp.StatusCode, r.Logs())
	}
	if a := tr.snapshot(); len(a) != 1 || a[0].Signal != "logs" || a[0].Payload == nil || a[0].At.IsZero() {
		t.Errorf("arrivals = %+v", a)
	}
}

// HoldFirst holds the first model call alone, and notes when the last was answered.
func TestHoldFirstOffline(t *testing.T) {
	var h HoldFirst
	srv := httptest.NewServer(h.Wrap(200*time.Millisecond, func(r *http.Request) bool { return r.Method == http.MethodPost },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	took := func(method string) time.Duration {
		t.Helper()
		start := time.Now()
		req, _ := http.NewRequest(method, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return time.Since(start)
	}
	if d := took("GET"); d > 150*time.Millisecond {
		t.Errorf("a request that is no model call was held %v", d)
	}
	if d := took("POST"); d < 200*time.Millisecond {
		t.Errorf("the first model call was held %v", d)
	}
	if d := took("POST"); d > 150*time.Millisecond {
		t.Errorf("the second model call was held %v", d)
	}
	if time.Since(h.Answered()) > time.Second {
		t.Errorf("answered at %v", h.Answered())
	}
}
