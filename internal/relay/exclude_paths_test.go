package relay

import (
	"encoding/json"
	"slices"
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

// pathExcludedJSON is a reference filter through protojson that pathExcluded must agree with.
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
			got, want := pathExcluded(msg, patterns), pathExcludedJSON(msg, patterns)
			if len(patterns) == 0 {
				want = false
			}
			if got != want {
				t.Errorf("%s, %v: pathExcluded = %v, the JSON walk %v", name, patterns, got, want)
			}
			if len(patterns) == 1 && patterns[0] == "secrets" && got != withheld[name] {
				t.Errorf("%s: withheld = %v, want %v", name, got, withheld[name])
			}
		}
	}
}
