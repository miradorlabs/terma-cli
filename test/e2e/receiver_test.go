package e2e

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Wire-level regression: retaining names alone loses the fields we need to
// validate. Both encodings must preserve the complete resource/scope/record.
func TestReceiverPreservesTelemetry(t *testing.T) {
	cases := []struct {
		name, body string
		message    func() proto.Message
		handler    func(*Receiver) http.HandlerFunc
	}{
		{"logs", `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"fixture"}}]},"scopeLogs":[{"scope":{"name":"scope","version":"1"},"logRecords":[{"timeUnixNano":"100","observedTimeUnixNano":"101","severityNumber":9,"eventName":"fixture.event","traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","body":{"stringValue":"hello"},"attributes":[{"key":"count","value":{"intValue":"42"}},{"key":"nested","value":{"arrayValue":{"values":[{"boolValue":true}]}}}]}]}]}]}`, func() proto.Message { return &collogspb.ExportLogsServiceRequest{} }, func(r *Receiver) http.HandlerFunc { return r.handleLogs }},
		{"traces", `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"fixture"}}]},"scopeSpans":[{"scope":{"name":"scope","version":"1"},"spans":[{"name":"fixture.span","traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","parentSpanId":"0807060504030201","startTimeUnixNano":"100","endTimeUnixNano":"200","status":{"code":2,"message":"failed"},"events":[{"name":"tool.output","timeUnixNano":"150","attributes":[{"key":"output","value":{"stringValue":"hello"}}]}],"links":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0807060504030201"}]}]}]}]}`, func() proto.Message { return &coltracepb.ExportTraceServiceRequest{} }, func(r *Receiver) http.HandlerFunc { return r.handleTraces }},
		{"metrics", `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"fixture"}}]},"scopeMetrics":[{"scope":{"name":"scope","version":"1"},"metrics":[{"name":"fixture.tokens","unit":"tokens","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[{"startTimeUnixNano":"100","timeUnixNano":"200","asInt":"42","attributes":[{"key":"type","value":{"stringValue":"input"}}]}]}},{"name":"fixture.latency","unit":"ms","histogram":{"aggregationTemporality":2,"dataPoints":[{"startTimeUnixNano":"100","timeUnixNano":"200","count":"2","sum":42,"explicitBounds":[10],"bucketCounts":["1","1"],"min":2,"max":40}]}}]}]}]}`, func() proto.Message { return &colmetricspb.ExportMetricsServiceRequest{} }, func(r *Receiver) http.HandlerFunc { return r.handleMetrics }},
	}
	for _, tc := range cases {
		for _, format := range []string{"json", "protobuf"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				// OTLP/JSON carries ids as hex, which plain protojson would read as base64.
				want := tc.message()
				if err := protojson.Unmarshal(otlpJSONIDs([]byte(tc.body)), want); err != nil {
					t.Fatal(err)
				}
				body := []byte(tc.body)
				if format == "protobuf" {
					var err error
					body, err = proto.Marshal(want)
					if err != nil {
						t.Fatal(err)
					}
				}
				r := &Receiver{}
				req := httptest.NewRequest("POST", "/v1/"+tc.name, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/"+format)
				req.Header.Set("Authorization", "Bearer fixture")
				response := httptest.NewRecorder()
				tc.handler(r)(response, req)
				requests := r.Requests()
				if response.Code != 200 || len(requests) != 1 {
					t.Fatalf("decode: %d, %d requests", response.Code, len(requests))
				}
				got := requests[0]
				if got.Path != "/v1/"+tc.name || got.Authorization != "Bearer fixture" || got.DecodeError != nil || !proto.Equal(got.Payload, want) {
					t.Fatal("receiver lost export fields")
				}
				switch tc.name {
				case "logs":
					if len(r.Logs()) != 1 || !proto.Equal(r.Logs()[0].Proto, want.(*collogspb.ExportLogsServiceRequest).ResourceLogs[0].ScopeLogs[0].LogRecords[0]) {
						t.Fatal("lost log fields")
					}
					if l := r.Logs()[0]; l.EventName != "fixture.event" || l.TraceID != "0102030405060708090a0b0c0d0e0f10" || l.Attrs["nested"] != "[true]" {
						t.Fatalf("flattened record: event %q trace %q attrs %v", l.EventName, l.TraceID, l.Attrs)
					}
				case "traces":
					if len(r.Spans()) != 1 || !proto.Equal(r.Spans()[0].Proto, want.(*coltracepb.ExportTraceServiceRequest).ResourceSpans[0].ScopeSpans[0].Spans[0]) {
						t.Fatal("lost span fields")
					}
				case "metrics":
					if len(r.Metrics()) != 2 || !proto.Equal(r.Metrics()[1].Proto, want.(*colmetricspb.ExportMetricsServiceRequest).ResourceMetrics[0].ScopeMetrics[0].Metrics[1]) {
						t.Fatal("lost metric values/buckets")
					}
				}
			})
		}
	}
}
