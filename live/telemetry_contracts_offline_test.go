package live

import (
	"fmt"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func mustReject(t *testing.T, want string, check func(contractReporter)) {
	t.Helper()
	var failures contractFailures
	check(&failures)
	if !strings.Contains(strings.Join(failures, "\n"), want) {
		t.Fatalf("want failure containing %q, got %v", want, failures)
	}
}

func TestTelemetryMissingSignalsFail(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			for _, surface := range []string{"/v1/logs", "/v1/traces", "/v1/metrics", "tool_result", "token.usage"} {
				if harness == "codex" && surface == "token.usage" {
					surface = "token_usage"
				}
				mustReject(t, surface, func(r contractReporter) {
					if harness == "claude" {
						checkClaudeTelemetry(r, telemetryEvidence{}, "s", "p", false)
					} else {
						checkCodexTelemetry(r, telemetryEvidence{}, "s", "p", false)
					}
				})
			}
		})
	}
}

func TestTelemetryEveryExportNeedsAuthorization(t *testing.T) {
	e := telemetryEvidence{requests: []ExportRequest{{Path: "/v1/logs", Authorization: "Bearer " + liveKey}, {Path: "/v1/traces", Authorization: "Bearer " + liveKey}, {Path: "/v1/metrics", Authorization: "Bearer " + liveKey}}}
	checkExportRequests(t, e)
	e.requests[2].Authorization = "Bearer wrong"
	mustReject(t, "wrong/missing authorization", func(r contractReporter) { checkExportRequests(r, e) })
	e.requests[2].Authorization = "Bearer " + liveKey
	e.requests[2].DecodeError = fmt.Errorf("malformed")
	mustReject(t, "invalid OTLP", func(r contractReporter) { checkExportRequests(r, e) })
}

func TestTelemetryChecksLaterRecordsAndSession(t *testing.T) {
	good := LogRecord{Time: time.Now(), Resource: map[string]string{"service.name": "fixture"}, Attrs: map[string]string{"event.name": "tool_result", "session.id": "s", "event.timestamp": time.Now().Format(time.RFC3339Nano), "tool_use_id": "call"}}
	e := telemetryEvidence{logs: []LogRecord{good}}
	e.logsFor(t, "tool_result", "session.id", "s", 1, "tool_use_id")
	// A healthy first record must not hide a later broken record.
	broken := good
	broken.Attrs = map[string]string{"event.name": "tool_result", "session.id": "s", "event.timestamp": good.Attrs["event.timestamp"]}
	e.logs = append(e.logs, broken)
	mustReject(t, "tool_use_id", func(r contractReporter) { e.logsFor(r, "tool_result", "session.id", "s", 1, "tool_use_id") })
	mustReject(t, "got 0 records", func(r contractReporter) { e.logsFor(r, "tool_result", "session.id", "other", 1, "tool_use_id") })
}

func TestTelemetryMetricValuesAndBuckets(t *testing.T) {
	sum := &metricspb.Sum{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, IsMonotonic: true, DataPoints: []*metricspb.NumberDataPoint{{StartTimeUnixNano: 100, TimeUnixNano: 200, Value: &metricspb.NumberDataPoint_AsInt{AsInt: 24}}}}
	m := &metricspb.Metric{Name: "tokens", Data: &metricspb.Metric_Sum{Sum: sum}}
	e := telemetryEvidence{metrics: []Metric{{Proto: m, Resource: map[string]string{"mirador.project.id": "p"}}}}
	checkSumMetric(t, e, "tokens", nil, 24, "p")
	sum.DataPoints[0].Value = &metricspb.NumberDataPoint_AsInt{AsInt: 12}
	mustReject(t, "want 24", func(r contractReporter) { checkSumMetric(r, e, "tokens", nil, 24, "p") })
	sum.AggregationTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	mustReject(t, "delta sum", func(r contractReporter) { checkSumMetric(r, e, "tokens", nil, 24, "p") })
	attrs := []*commonpb.KeyValue{}
	for _, key := range []string{"auth_mode", "originator", "session_source", "model", "app.version"} {
		attrs = append(attrs, &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "fixture"}}})
	}
	total := 24.0
	p := &metricspb.HistogramDataPoint{Attributes: attrs, StartTimeUnixNano: 100, TimeUnixNano: 200, Count: 1, Sum: &total, ExplicitBounds: []float64{10}, BucketCounts: []uint64{0, 1}}
	m.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, DataPoints: []*metricspb.HistogramDataPoint{p}}}
	e.metrics[0].Resource["service.name"] = "fixture"
	checkCodexHistogram(t, e, "tokens", nil, 24)
	p.BucketCounts = []uint64{0, 0}
	mustReject(t, "buckets", func(r contractReporter) { checkCodexHistogram(r, e, "tokens", nil, 24) })
	p.BucketCounts = []uint64{0, 1}
	p.Attributes = p.Attributes[1:]
	mustReject(t, "auth_mode", func(r contractReporter) { checkCodexHistogram(r, e, "tokens", nil, 24) })
}

func TestTelemetryTraceCorrelation(t *testing.T) {
	trace := []byte("0123456789abcdef")
	rootID := []byte("01234567")
	root := Span{Name: "turn", Attrs: map[string]string{"session.id": "s"}, Resource: map[string]string{"service.name": "fixture", "mirador.project.id": "p"}, Proto: &tracepb.Span{TraceId: trace, SpanId: rootID, StartTimeUnixNano: 100, EndTimeUnixNano: 200}}
	child := root
	child.Name = "request"
	child.Proto = &tracepb.Span{TraceId: trace, SpanId: []byte("12345678"), ParentSpanId: rootID, StartTimeUnixNano: 120, EndTimeUnixNano: 180}
	e := telemetryEvidence{spans: []Span{root, child}}
	checkSpans(t, e, "session.id", "s", "p", "turn", "request")
	child.Proto.ParentSpanId = []byte("orphaned")
	mustReject(t, "parent span", func(r contractReporter) { checkSpans(r, e, "session.id", "s", "p", "turn", "request") })
	child.Proto.ParentSpanId = rootID
	child.Proto.EndTimeUnixNano = 50
	mustReject(t, "timing", func(r contractReporter) { checkSpans(r, e, "session.id", "s", "p", "turn", "request") })
}

func TestTelemetryRedactionScansRawPayloads(t *testing.T) {
	payload := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: telemetryPrompt}}}}}}}}}
	e := telemetryEvidence{requests: []ExportRequest{{Path: "/v1/logs", Payload: payload}}}
	// The content is in the native envelope, not in our flattened attribute maps.
	mustReject(t, "excluded content leaked", func(r contractReporter) { checkRedaction(r, e, "codex") })
	payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "<REDACTED>"}}
	checkRedaction(t, e, "codex")
}

func TestCodexUsageSelectionKeepsMalformedRecords(t *testing.T) {
	base := map[string]string{"event.name": "codex.sse_event", "event.kind": "response.completed", "conversation.id": "s"}
	record := func(extra map[string]string) LogRecord {
		a := map[string]string{}
		for k, v := range base {
			a[k] = v
		}
		for k, v := range extra {
			a[k] = v
		}
		return LogRecord{Attrs: a}
	}
	receiver := &Receiver{logs: []LogRecord{
		record(map[string]string{"duration_ms": "1"}),
		record(map[string]string{"input_token_count": "12", "output_token_count": "4", "cached_token_count": "3"}),
		record(map[string]string{"output_token_count": "4"}),
		record(nil),
	}}
	sb := &Sandbox{Receiver: receiver}
	_, usage := sb.CodexLogs("s", 0)
	if len(usage) != 3 {
		t.Fatalf("timing must be excluded, malformed usage retained: got %d records", len(usage))
	}
	mustReject(t, "input_token_count", func(r contractReporter) { checkCodexCompleted(r, usage) })
}

// A missing session end fails the contract unless the run names Codex's upstream
// SessionEnd race in TERMA_LIVE_KNOWN_UPSTREAM, which only pull-request CI does; the
// session start stays required either way, and nothing else is tolerated by that name.
func TestHookLifecycleToleratesOnlyTheNamedUpstreamFailure(t *testing.T) {
	start := LogRecord{Resource: map[string]string{"service.name": "terma-cli", "mirador.project.id": "p"},
		Attrs: map[string]string{"event.name": "terma.session.start", "session.id": "s", "tool": "codex", "project_id": "p"}}
	e := telemetryEvidence{logs: []LogRecord{start}}

	t.Setenv("TERMA_LIVE_KNOWN_UPSTREAM", "")
	mustReject(t, "terma.session.end", func(r contractReporter) {
		checkHookLifecycle(r, e, "s", "p", knownUpstream(upstreamCodexSessionEnd))
	})

	t.Setenv("TERMA_LIVE_KNOWN_UPSTREAM", "something-else,"+upstreamCodexSessionEnd)
	checkHookLifecycle(t, e, "s", "p", knownUpstream(upstreamCodexSessionEnd))
	mustReject(t, "terma.session.start", func(r contractReporter) {
		checkHookLifecycle(r, telemetryEvidence{}, "s", "p", knownUpstream(upstreamCodexSessionEnd))
	})
}
