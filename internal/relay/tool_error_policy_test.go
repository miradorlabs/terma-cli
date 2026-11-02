package relay

import (
	"fmt"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestToolErrorsFollowToolContentPolicy(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			for _, event := range []string{"tool_result", "claude_code.tool_result", "api_request"} {
				lr := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv("event.name", event), kv("error", "tool error")}}
				p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{lr}}}}}}}
				testRules.withhold(p, flags[0], flags[1], map[string]int{})
				want := flags[1]
				if event == "api_request" {
					want = flags[0]
				}
				if got := attrString(lr.Attributes, "error") != ""; got != want {
					t.Errorf("%s: error present=%v, want %v", event, got, want)
				}
			}
			for _, operation := range []string{"execute_tool", "chat"} {
				sp := &tracepb.Span{Attributes: []*commonpb.KeyValue{kv("gen_ai.operation.name", operation)},
					Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "tool error"},
					Events: []*tracepb.Span_Event{{Name: "gen_ai.client.operation.exception", Attributes: []*commonpb.KeyValue{kv("exception.message", "tool error"), kv("exception.type", "ToolError")}}}}
				p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}}
				testRules.withhold(p, flags[0], flags[1], map[string]int{})
				want := flags[0] && flags[1] || operation == "execute_tool" && flags[1]
				if got := sp.Status.Message != ""; got != want {
					t.Errorf("%s: status present=%v, want %v", operation, got, want)
				}
				if got := attrString(sp.Events[0].Attributes, "exception.message") != ""; got != want {
					t.Errorf("%s: exception present=%v, want %v", operation, got, want)
				}
				if sp.Status.Code != tracepb.Status_STATUS_CODE_ERROR {
					t.Fatal("failure flag changed")
				}
			}
		})
	}
}
