package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestCaptureWithholdsProviderErrorAndOtherAttributeChannels(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
		span := &tracepb.Span{Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "PRIVATE_CONTENT"}, Links: []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{kv("gen_ai.prompt", "PRIVATE_CONTENT")}}}}
		scope := &commonpb.InstrumentationScope{Attributes: []*commonpb.KeyValue{kv("gen_ai.prompt", "PRIVATE_CONTENT")}}
		p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{span}}}}}}}
		testRules.withhold(p, flags[0], flags[1], map[string]int{})
		if !flags[0] || !flags[1] {
			if span.Status.Message != "" || span.Status.Code != tracepb.Status_STATUS_CODE_ERROR {
				t.Fatal("provider error text survived or status code changed")
			}
		}
		if !flags[0] && (len(scope.Attributes) != 0 || len(span.Links[0].Attributes) != 0) {
			t.Fatal("prompt escaped through scope/link attributes")
		}
	}
}

func TestQueuedExportsRespectSignalAndCoverageChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy Policy
	}{
		{"signal disabled", Policy{Signals: []string{"metrics"}, IncludePrompts: true, IncludeToolContent: true}},
		{"left global coverage", Policy{RequireClaim: true, IncludePrompts: true, IncludeToolContent: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("disallowed queued export reached upstream")
				w.WriteHeader(200)
			}))
			defer host.Close()
			policy := test.policy
			policy.Endpoint = host.URL
			policy.Key = "key"
			r := newRelay(Options{Dir: t.TempDir(), Resolve: func(claim.Claim) (Policy, error) { return policy, nil }})
			m := logsOf("session", 1)
			m.ResourceLogs[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv(AttributionAttr, "catch-all")}}
			body, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			rt := route{project: "team", tool: "codex"}
			e := newEntry(time.Now(), Logs, 1)
			if err := r.outbox.put(rt, e, body); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			waitFor(t, func() bool { return r.Stats().Snapshot().Counters["dropped.policy_signal_or_content.logs"] == 1 })
			entries, err := r.outbox.list(rt)
			if err != nil || len(entries) != 0 {
				t.Fatalf("disallowed export remained queued: %v %v", entries, err)
			}
		})
	}
}

func TestQueuedCapturePolicyFiltersPathsAndCorruptBodies(t *testing.T) {
	r := newRelay(Options{})
	m := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Attributes: []*commonpb.KeyValue{kv("tool_input", `{"file_path":"src/secrets/passwords.txt"}`)}}}}}}}}
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if got, excluded := r.withholdQueued(Traces, b, Policy{Excludes: excluding("**/secrets/**")}); got != nil || excluded != 1 {
		t.Fatalf("queued excluded path survived, or was not counted as one (%d)", excluded)
	}
	if got, _ := r.withholdQueued(Traces, []byte{0xff}, Policy{}); got != nil {
		t.Fatal("uncheckable body was forwarded")
	}

	// A queued batch keeps the spans that name no excluded file.
	spans := m.ResourceSpans[0].ScopeSpans[0]
	spans.Spans = append(spans.Spans, &tracepb.Span{Name: "Read README.md", Attributes: []*commonpb.KeyValue{kv("file_path", "README.md")}})
	if b, err = proto.Marshal(m); err != nil {
		t.Fatal(err)
	}
	got, excluded := r.withholdQueued(Traces, b, Policy{Excludes: excluding("**/secrets/**")})
	var kept tracepb.TracesData
	if err := proto.Unmarshal(got, &kept); err != nil || excluded != 1 {
		t.Fatalf("excluded %d, err %v; want 1 excluded and the rest sent", excluded, err)
	}
	if left := kept.ResourceSpans[0].ScopeSpans[0].Spans; len(left) != 1 || left[0].Name != "Read README.md" {
		t.Fatalf("kept %v, want only the README span", left)
	}
}
