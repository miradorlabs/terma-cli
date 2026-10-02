package relay

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// pathExcludedJSON is a reference filter through protojson that dropExcluded must agree with.
func pathExcludedJSON(msg proto.Message, patterns []string) bool {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return true
	}
	var data any
	if json.Unmarshal(b, &data) != nil {
		return true
	}
	pol := config.Policy{ExcludePaths: patterns}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if key, ok := x["key"].(string); ok {
				value, _ := x["value"].(map[string]any)
				if pol.HasExcludedPath(map[string]any{key: value}, "") {
					return true
				}
			}
			for _, val := range x {
				if walk(val) {
					return true
				}
			}
		case []any:
			if slices.ContainsFunc(x, walk) {
				return true
			}
		}
		return false
	}
	return walk(data)
}

func TestPathExcludedMatchesTheJSONWalk(t *testing.T) {
	secret := "secrets/prod.env"
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	kvl := func(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
	}
	arr := func(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
	}
	attr := func(k string, v *commonpb.AnyValue) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{{Key: k, Value: v}}
	}
	logs := func(res, scope, rec []*commonpb.KeyValue, body *commonpb.AnyValue) proto.Message {
		return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{Attributes: res},
			ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Attributes: scope}, LogRecords: []*logspb.LogRecord{{Attributes: rec, Body: body}}}}}}}
	}
	cases := map[string]proto.Message{
		"record path":            logs(nil, nil, attr("file.path", str(secret)), nil),
		"record other path":      logs(nil, nil, attr("file.path", str("src/main.go")), nil),
		"not a path key":         logs(nil, nil, attr("gen_ai.prompt", str(secret)), nil),
		"resource cwd":           logs(attr("process.cwd", str("secrets")), nil, nil, nil),
		"scope directory":        logs(nil, attr("workdirectory", str("secrets")), nil, nil),
		"files array":            logs(nil, nil, attr("files", arr(str("a.go"), str(secret))), nil),
		"JSON-encoded arguments": logs(nil, nil, attr("tool.arguments", str(`{"file_path":"`+secret+`"}`)), nil),
		"nested kvlist":          logs(nil, nil, attr("tool", kvl(&commonpb.KeyValue{Key: "path", Value: str(secret)})), nil),
		"body kvlist":            logs(nil, nil, nil, kvl(&commonpb.KeyValue{Key: "target_path", Value: str(secret)})),
		"body string":            logs(nil, nil, nil, str(secret)),
		"empty key":              logs(nil, nil, attr("", str(`{"path":"`+secret+`"}`)), nil),
		"int under a path key":   logs(nil, nil, attr("path", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}), nil),
		"span event": &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			Events: []*tracepb.Span_Event{{Attributes: attr("file_path", str(secret))}}}}}}}}},
		"span link": &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			Links: []*tracepb.Span_Link{{Attributes: attr("path", str(secret))}}}}}}}}},
		"metric exemplar": &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{
				Exemplars: []*metricspb.Exemplar{{FilteredAttributes: attr("file.path", str(secret))}}}}}}}}}}}}},
	}
	withheld := map[string]bool{"record path": true, "resource cwd": true, "scope directory": true, "files array": true,
		"JSON-encoded arguments": true, "nested kvlist": true, "body kvlist": true, "span event": true, "span link": true, "metric exemplar": true}
	for name, msg := range cases {
		for _, patterns := range [][]string{{"secrets"}, {"7"}, nil} {
			got, want := dropExcluded(&part{msg: proto.Clone(msg)}, excluding(patterns...)) > 0, pathExcludedJSON(msg, patterns)
			if len(patterns) == 0 {
				want = false
			}
			if got != want {
				t.Errorf("%s, %v: dropExcluded = %v, the JSON walk %v", name, patterns, got, want)
			}
			if len(patterns) == 1 && patterns[0] == "secrets" && got != withheld[name] {
				t.Errorf("%s: withheld = %v, want %v", name, got, withheld[name])
			}
		}
	}
}

// excluding is the matcher the daemon builds for patterns.
func excluding(patterns ...string) func(any) bool {
	if len(patterns) == 0 {
		return nil
	}
	p := config.Policy{ExcludePaths: patterns}
	return func(v any) bool { return p.HasExcludedPath(v, "") }
}

// An excluded path takes only the records that name it: the session's other tool calls,
// model calls and spans in the same export still reach the project.
func TestPathExclusionDropsOnlyTheRecordsNamingIt(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	pol := Policy{Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true, Excludes: excluding("secrets/**")}
	r, srv := f.relay(t, u, map[string]Policy{"p1": pol})

	rec := func(event string, extra ...*commonpb.KeyValue) *logspb.LogRecord {
		return &logspb.LogRecord{Attributes: append([]*commonpb.KeyValue{kv("session.id", "A"), kv("event.name", event)}, extra...)}
	}
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		rec("tool_result", kv("tool_parameters", `{"file_path":"secrets/app.env"}`)),
		rec("tool_result", kv("tool_parameters", `{"file_path":"README.md"}`)),
		rec("api_request", kv("model", "sonnet")),
	}}}}}}
	span := func(name string, extra ...*commonpb.KeyValue) *tracepb.Span {
		return &tracepb.Span{TraceId: make([]byte, 16), SpanId: []byte(name[:8]), Name: name,
			Attributes: append([]*commonpb.KeyValue{kv("session.id", "A")}, extra...)}
	}
	traces := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		span("tool.read.secret", kv("file_path", "secrets/app.env")),
		span("tool.write.e2e", kv("file_path", "e2e/p6.txt")),
	}}}}}}
	for path, msg := range map[string]proto.Message{"/v1/logs": logs, "/v1/traces": traces} {
		body, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if code := post(t, srv, path, body, "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("%s = %d", path, code)
		}
	}
	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["forwarded.logs"] == 2 && c["forwarded.traces"] == 1
	})
	c := r.Stats().Snapshot().Counters
	if c["dropped.policy_path.logs"] != 1 || c["dropped.policy_path.traces"] != 1 {
		t.Fatalf("stats = %v, want one log and one span dropped for the path", c)
	}
	got, _ := u.logs(t)
	for _, lr := range got["Bearer key-p1"] {
		if strings.Contains(attr(lr.Attributes, "tool_parameters"), "secrets") {
			t.Fatalf("the excluded file's record left: %v", lr.Attributes)
		}
	}
}

// A shell call whose command names an excluded file goes whole, its output with it, and
// counts as a path drop.
func TestPathExclusionDropsShellCommandsNamingIt(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	pol := Policy{Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true, Excludes: excluding("secrets/**")}
	r, srv := f.relay(t, u, map[string]Policy{"p1": pol})

	rec := func(extra ...*commonpb.KeyValue) *logspb.LogRecord {
		return &logspb.LogRecord{Attributes: append([]*commonpb.KeyValue{kv("session.id", "A"), kv("event.name", "codex.tool_result")}, extra...)}
	}
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		rec(kv("arguments", `{"command":["bash","-lc","cat secrets/app.env"],"workdir":"/repo"}`), kv("output", "API_KEY=hunter2")),
		rec(kv("tool_parameters", `{"bash_command":"cat","full_command":"cat secrets/app.env"}`), kv("output", "API_KEY=hunter2")),
		rec(kv("arguments", `{"command":["bash","-lc","cat README.md"]}`), kv("output", "readme")),
	}}}}}}
	body, err := proto.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("/v1/logs = %d", code)
	}
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	if c := r.Stats().Snapshot().Counters; c["dropped.policy_path.logs"] != 2 {
		t.Fatalf("stats = %v, want both shell reads of the excluded file dropped for the path", c)
	}
	got, _ := u.logs(t)
	for _, lr := range got["Bearer key-p1"] {
		for _, a := range lr.Attributes {
			if strings.Contains(a.GetValue().GetStringValue(), "secrets") || strings.Contains(a.GetValue().GetStringValue(), "hunter2") {
				t.Fatalf("the excluded file's command or output left: %v", lr.Attributes)
			}
		}
	}
}
