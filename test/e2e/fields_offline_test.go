package e2e

import (
	"maps"
	"slices"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func res(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", service), str("host.arch", "arm64")}}
}

// The census names each key's surface, a GenAI span's by its operation, and the kinds of
// value it carried, a number sent as a string a number; terma's own records are not the
// harness's.
func TestObserveFields(t *testing.T) {
	fieldsMu.Lock()
	clear(fieldSeen)
	fieldsMu.Unlock()
	b := Binary{Harness: "opencode", Version: "1.2.3"}
	ObserveFields(b, []ExportRequest{
		{Payload: &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{
			{Resource: res("opencode"), ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
				{Attributes: []*commonpb.KeyValue{str("event.name", "tool_result"), str("duration_ms", "12"), str("tool_name", "bash")}},
				{Attributes: []*commonpb.KeyValue{str("duration_ms", "slow")}}, // no event: named by its scope
				// Codex: the callsite in EventName, the event's name in its attribute.
				{EventName: "event otel/src/events/session_telemetry.rs:785", Attributes: []*commonpb.KeyValue{str("event.name", "codex.api_request"), num("attempt", 1)}},
				{EventName: "event otel/src/other.rs:12", Attributes: []*commonpb.KeyValue{str("model", "gpt")}},
			}}}},
			{Resource: res("terma-cli"), ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
				{Attributes: []*commonpb.KeyValue{str("event.name", "terma.session.start")}},
			}}}},
		}}},
		{Payload: &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: res("opencode"),
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "chat gpt-6", Attributes: []*commonpb.KeyValue{str("gen_ai.operation.name", "chat"), num("gen_ai.usage.input_tokens", 4)},
				Events: []*tracepb.Span_Event{{Name: "tool.output", Attributes: []*commonpb.KeyValue{str("content", "out")}},
					{Name: "event otel/src/tool_result.rs:54", Attributes: []*commonpb.KeyValue{str("auth_mode", "ApiKey")}}}}}}}}}}},
		{Payload: &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: res("opencode"),
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "tokens", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				DataPoints: []*metricspb.NumberDataPoint{{Attributes: []*commonpb.KeyValue{str("type", "input"), str("cached", "true")}}}}}}}}}}}}},
	})
	got := map[string][]string{}
	fieldsMu.Lock()
	for id, kinds := range fieldSeen {
		got[id.surface+" "+id.key] = slices.Sorted(maps.Keys(kinds))
	}
	clear(fieldSeen)
	fieldsMu.Unlock()
	want := map[string][]string{
		"resource service.name":                                               {KindText},
		"resource host.arch":                                                  {KindText},
		"logs/tool_result event.name":                                         {KindText},
		"logs/tool_result duration_ms":                                        {KindNumber},
		"logs/tool_result tool_name":                                          {KindText},
		"logs/(scope ) duration_ms":                                           {KindText},
		"logs/codex.api_request event.name":                                   {KindText},
		"logs/codex.api_request attempt":                                      {KindNumber},
		"logs/event otel/src/other.rs model":                                  {KindText},
		"traces/chat {target} gen_ai.operation.name":                          {KindText},
		"traces/chat {target} gen_ai.usage.input_tokens":                      {KindNumber},
		"traces/chat {target}/events/tool.output content":                     {KindText},
		"traces/chat {target}/events/event otel/src/tool_result.rs auth_mode": {KindText},
		"metrics/tokens type":                                                 {KindText},
		"metrics/tokens cached":                                               {KindBool},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		for k, v := range got {
			if !slices.Equal(v, want[k]) {
				t.Errorf("%s: %v, want %v", k, v, want[k])
			}
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: not observed", k)
			}
		}
	}
}

// The census counts a string as a number as the relay's numericOrBool does, or the digest
// would miss a key the relay withholds: a long digit string is text to it.
func TestKindOfMatchesTheRelay(t *testing.T) {
	sv := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	for s, want := range map[string]string{
		"3": KindNumber, "-12.5": KindNumber, "true": KindBool, "": KindText, "3 files": KindText,
		"0x1f": KindText, "NaN": KindText, "Inf": KindText, "1_000": KindText, "1p3": KindText,
		"123456789012345678901234567890123": KindText, // 33 digits: past the relay's cap
	} {
		if got := kindOf(sv(s)); got != want {
			t.Errorf("kindOf(%q) = %s, want %s", s, got, want)
		}
	}
}
