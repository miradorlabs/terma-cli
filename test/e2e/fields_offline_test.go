package e2e

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
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
	point := &metricspb.NumberDataPoint{Attributes: []*commonpb.KeyValue{str("type", "input"), str("cached", "true")},
		Exemplars: []*metricspb.Exemplar{{FilteredAttributes: []*commonpb.KeyValue{str("trace.note", "x")}}}}
	metric := &metricspb.Metric{Name: "tokens", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{point}}}}
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
			ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Attributes: []*commonpb.KeyValue{str("scope.flag", "on")}}, Spans: []*tracepb.Span{{Name: "chat gpt-6", Attributes: []*commonpb.KeyValue{str("gen_ai.operation.name", "chat"), num("gen_ai.usage.input_tokens", 4)},
				Links: []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{str("link.kind", "parent")}}},
				Events: []*tracepb.Span_Event{{Name: "tool.output", Attributes: []*commonpb.KeyValue{str("content", "out")}},
					{Name: "event otel/src/tool_result.rs:54", Attributes: []*commonpb.KeyValue{str("auth_mode", "ApiKey")}}}},
				// A span whose name reads like a span event's surface is still a span.
				{Name: "GET /v1/events/x", Attributes: []*commonpb.KeyValue{str("http.method", "GET")}}}}}}}}},
		{Payload: &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: res("opencode"),
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{metric}}}}}}},
	})
	got, sites := map[string][]string{}, map[string]classQuery{}
	fieldsMu.Lock()
	for id, f := range fieldSeen {
		got[id.surface+" "+id.key] = slices.Sorted(maps.Keys(f.kinds))
		sites[id.surface+" "+id.key] = f.query
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
		"scope scope.flag":                                                    {KindText},
		"traces/chat {target}/links link.kind":                                {KindText},
		"metrics/tokens/exemplars trace.note":                                 {KindText},
		"traces/GET /v1/events/x http.method":                                 {KindText},
	}
	// Where each key sits, as the relay is asked about it.
	for surfaceKey, want := range map[string]classQuery{
		"resource service.name":                                               {Site: "resource", Key: "service.name"},
		"logs/tool_result tool_name":                                          {Site: "record", Key: "tool_name"},
		"traces/chat {target}/events/tool.output content":                     {Site: "event", Event: "tool.output", Key: "content"},
		"traces/chat {target}/events/event otel/src/tool_result.rs auth_mode": {Site: "event", Event: "event otel/src/tool_result.rs", Key: "auth_mode"},
		"traces/GET /v1/events/x http.method":                                 {Site: "record", Key: "http.method"},
		"metrics/tokens/exemplars trace.note":                                 {Site: "record", Key: "trace.note"},
	} {
		if sites[surfaceKey] != want {
			t.Errorf("%s sits at %+v, want %+v", surfaceKey, sites[surfaceKey], want)
		}
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

// A census the terma under test cannot classify is not written, and takes the earlier run's
// with it, so `make drift` never reads an old census as this run's.
func TestAnUnclassifiedCensusLeavesNoEarlierOne(t *testing.T) {
	fieldsMu.Lock()
	clear(fieldSeen)
	fieldSeen[fieldID{"opencode", "1.2.3", "resource", "service.name"}] = &seenField{kinds: map[string]bool{KindText: true}, query: classQuery{Site: "resource", Key: "service.name"}}
	fieldsMu.Unlock()
	t.Cleanup(func() { fieldsMu.Lock(); clear(fieldSeen); fieldsMu.Unlock() })
	dir := t.TempDir()
	path := filepath.Join(dir, "fields.json")
	if err := os.WriteFile(path, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFields(dir, filepath.Join(dir, "no-terma")); err == nil {
		t.Fatal("a census no terma classified was written")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the earlier census is still there: %v", err)
	}
}

// The census takes each key's class, and the kinds of value it keeps, from `terma relay
// classify`, asked where the key sits: a number on a resource is withheld when the relay keeps
// no kind of value there.
func TestCensusTakesWithholdingFromTheRelay(t *testing.T) {
	fieldsMu.Lock()
	clear(fieldSeen)
	fieldSeen[fieldID{"opencode", "1.2.3", "resource", "process.parent_pid"}] = &seenField{kinds: map[string]bool{KindNumber: true}, query: classQuery{Site: "resource", Key: "process.parent_pid"}}
	fieldsMu.Unlock()
	t.Cleanup(func() { fieldsMu.Lock(); clear(fieldSeen); fieldsMu.Unlock() })
	dir := t.TempDir()
	// A terma that checks the one query it is asked, and keeps nothing of it.
	terma := filepath.Join(dir, "terma")
	script := `#!/bin/sh
[ "$1" = --version ] && { echo "terma 9.9.9"; exit 0; }
in=$(cat)
[ "$in" = '[{"site":"resource","key":"process.parent_pid"}]' ] || { echo "unexpected query: $in" >&2; exit 1; }
echo '[{"class": "unclassified", "kept": []}]'
`
	if err := os.WriteFile(terma, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFields(dir, terma); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fields.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []FieldRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Class != "unclassified" || !Withheld(rows[0].Class, rows[0].Kinds, rows[0].Kept) || rows[0].Terma != "terma 9.9.9" {
		t.Errorf("census = %+v", rows)
	}
}
