package relay

import (
	"fmt"
	"maps"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// Between them the agents declare exactly what the relay's own tables held.
func TestComposedRulesMatchTheLegacyTables(t *testing.T) {
	for _, s := range []struct {
		name      string
		got, want []string
	}{
		{"prompt fields", testRules.promptFields, legacyPromptFields},
		{"prompt drop fields", testRules.promptDropFields, legacyPromptDropFields},
		{"prompt body events", testRules.promptBodyEvents, legacyPromptBodyEvents},
		{"resource prompt fields", testRules.resourcePromptFields, legacyResourcePromptFields},
		{"tool content fields", testRules.toolContentFields, legacyToolContentFields},
		{"tool content events", testRules.toolContentEvents, legacyToolContentEvents},
		{"start events", testRules.startEvents, []string{"codex.conversation_starts"}},
	} {
		if got, want := slices.Sorted(slices.Values(s.got)), slices.Sorted(slices.Values(s.want)); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", s.name, got, want)
		}
	}
	var keys, numericRejected []string
	for _, k := range testRules.sessionKeys {
		keys = append(keys, k.Attr)
		if k.RejectNumeric {
			numericRejected = append(numericRejected, k.Attr)
		}
	}
	if !slices.Equal(keys, legacySessionKeys) {
		t.Errorf("session key precedence = %q, want %q", keys, legacySessionKeys)
	}
	if !slices.Equal(numericRejected, []string{"thread.id", "thread_id"}) {
		t.Errorf("numeric ids rejected for %q", numericRejected)
	}
}

// A key an agent declares as content must not also be on the safe list, or a record
// from another agent would carry it out.
func TestNoDeclaredContentKeyIsSafe(t *testing.T) {
	for _, c := range testCapturers {
		r := c.CaptureRules()
		for _, key := range slices.Concat(r.PromptFields, r.PromptDropFields, r.ResourcePromptFields, r.ToolContentFields) {
			if safeKey(key) {
				t.Errorf("%T declares %q as content, and allow.go lists it safe", c, key)
			}
		}
	}
}

// Every known agent's shape is composed, supported or not: an unsupported agent's
// committed hooks still claim sessions, and its content must still be withheld.
func TestEveryExportingAgentDeclaresItsShape(t *testing.T) {
	reg := builtin.Agents()
	for _, a := range reg.All() {
		_, correlates := a.(shape.Correlator)
		_, captures := a.(shape.Capturer)
		if correlates != captures {
			t.Errorf("%s declares one half of its telemetry shape", a.Name())
		}
	}
	if n, m := len(testCorrelators), len(reg.With[shape.Capturer]()); n == 0 || n != m {
		t.Fatalf("%d correlators, %d capturers", n, m)
	}
}

// The composed rules withhold, record by record, exactly what the legacy tables did.
func TestComposedRulesWithholdAsTheLegacyTablesDid(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := range 3000 {
		prompts, toolContent := rng.Intn(2) == 0, rng.Intn(2) == 0
		msg := randomExport(rng)
		want, got := &part{msg: proto.Clone(msg)}, &part{msg: proto.Clone(msg)}
		wantU, gotU := map[string]int{}, map[string]int{}
		wantN := legacyWithhold(want, prompts, toolContent, wantU)
		gotN := testRules.withhold(got, prompts, toolContent, gotU)
		if wantN != gotN || !maps.Equal(wantU, gotU) || !proto.Equal(want.msg, got.msg) {
			t.Fatalf("case %d (prompts %v, tool content %v): changed %d, want %d; unclassified %v, want %v\n got %v\nwant %v",
				i, prompts, toolContent, gotN, wantN, gotU, wantU, got.msg, want.msg)
		}
		if logs, ok := msg.(*logspb.LogsData); ok {
			if got, want := testRules.conversationStart(&part{msg: logs}), legacyConversationStart(&part{msg: logs}); got != want {
				t.Fatalf("case %d: conversation start %v, want %v", i, got, want)
			}
			for _, rl := range logs.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						res := rl.GetResource().GetAttributes()
						if got, want := testRules.sessionOf(lr.Attributes, res), legacySessionOf(lr.Attributes, res); got != want {
							t.Fatalf("case %d: session %q, want %q", i, got, want)
						}
					}
				}
			}
		}
	}
}

var keyPool = func() []string {
	pool := slices.Concat(legacyPromptFields, legacyPromptDropFields, legacyResourcePromptFields, legacyToolContentFields,
		legacySessionKeys, []string{"event.name", "tool_name", "gen_ai.usage.input_tokens", "x.new_note", "blob", "seq"})
	return slices.Compact(slices.Sorted(slices.Values(pool)))
}()

func randomExport(rng *rand.Rand) proto.Message {
	value := func() string {
		return []string{"SECRET", "42", "[REDACTED]", "<REDACTED>", "", "0x1f"}[rng.Intn(6)]
	}
	attrs := func() []*commonpb.KeyValue {
		var out []*commonpb.KeyValue
		for range rng.Intn(6) {
			key := keyPool[rng.Intn(len(keyPool))]
			if key == "event.name" {
				out = append(out, kv(key, randomEvent(rng)))
				continue
			}
			if rng.Intn(8) == 0 {
				out = append(out, &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}})
				continue
			}
			out = append(out, kv(key, value()))
		}
		return out
	}
	resource := func() *resourcepb.Resource { return &resourcepb.Resource{Attributes: attrs()} }
	switch rng.Intn(3) {
	case 0:
		var recs []*logspb.LogRecord
		for range 1 + rng.Intn(3) {
			lr := &logspb.LogRecord{Attributes: attrs()}
			event := randomEvent(rng)
			lr.Attributes = append(lr.Attributes, kv("event.name", event))
			switch rng.Intn(5) {
			case 0:
				lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: event}}
			case 1:
				lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code." + event}}
			case 2:
				lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "SECRET"}}
			case 3:
				lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}
			}
			recs = append(recs, lr)
		}
		return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: resource(),
			ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Attributes: attrs()}, LogRecords: recs}}}}}
	case 1:
		sp := &tracepb.Span{Attributes: attrs(), Links: []*tracepb.Span_Link{{Attributes: attrs()}}}
		if rng.Intn(2) == 0 {
			sp.Status = &tracepb.Status{Message: "SECRET"}
		}
		for _, name := range []string{"tool.output", "tool.input", "exception", "note"}[:rng.Intn(5)] {
			sp.Events = append(sp.Events, &tracepb.Span_Event{Name: name, Attributes: attrs()})
		}
		return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: resource(),
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}
	default:
		pt := &metricspb.NumberDataPoint{Attributes: attrs(), Exemplars: []*metricspb.Exemplar{{FilteredAttributes: attrs()}}}
		return &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: resource(),
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
				{Name: "m", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{pt}}}},
				{Name: "h", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{Attributes: attrs()}}}}},
			}}}}}}
	}
}

func randomEvent(rng *rand.Rand) string {
	events := slices.Concat(legacyPromptBodyEvents, []string{"codex.conversation_starts", "user_prompt", "api_request", ""})
	return events[rng.Intn(len(events))]
}

// The policy matrix: for every agent, in repository and global mode, under each
// combination of the team's prompt and tool content policy, every field the agent
// declares as content leaves only when the policy allows it. Values are sentinels, so
// a field that leaves shows in the bytes upstream receives.
func TestPolicyMatrixPerAgent(t *testing.T) {
	for _, c := range testCapturers {
		agent := strings.TrimPrefix(fmt.Sprintf("%T", c), "*")
		session := "session.id"
		if cor, ok := c.(shape.Correlator); ok && len(cor.Correlation().SessionKeys) > 0 {
			session = cor.Correlation().SessionKeys[0].Attr
		}
		rules := c.CaptureRules()
		for _, global := range []bool{false, true} {
			for _, prompts := range []bool{false, true} {
				for _, toolContent := range []bool{false, true} {
					name := fmt.Sprintf("%s/global=%v/prompts=%v/tools=%v", agent, global, prompts, toolContent)
					t.Run(name, func(t *testing.T) {
						got := deliverThroughRelay(t, global, Policy{IncludePrompts: prompts, IncludeToolContent: toolContent}, session, rules)
						check := func(kind string, keys []string, allowed bool) {
							for _, key := range keys {
								leaked := strings.Contains(got, sentinel(key))
								if kind == "tool content event" {
									leaked = strings.Contains(got, key)
								}
								if leaked != allowed {
									t.Errorf("%s %q: in the export %v, policy allows %v", kind, key, leaked, allowed)
								}
							}
						}
						check("prompt field", rules.PromptFields, prompts)
						check("prompt drop field", rules.PromptDropFields, prompts)
						// A body is free text: it is kept only when no content is withheld.
						check("prompt body event", rules.PromptBodyEvents, prompts && toolContent)
						check("resource prompt field", rules.ResourcePromptFields, prompts)
						check("tool content field", rules.ToolContentFields, toolContent)
						check("tool content event", rules.ToolContentEvents, toolContent)
					})
				}
			}
		}
	}
}

func sentinel(key string) string { return "SENTINEL<" + key + ">" }

// deliverThroughRelay posts one log export and one trace export carrying every content
// field in rules through a running relay, in repository mode (a claimed session) or
// global mode (no claim, the catch-all), and returns what upstream received.
func deliverThroughRelay(t *testing.T, global bool, pol Policy, sessionKey string, rules shape.CaptureRules) string {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	pol.Endpoint, pol.Key = u.srv.URL, "key"
	opts := Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }}
	sid := "S"
	if global {
		opts.CatchAll = func() (claim.Claim, bool) { return claim.Claim{ProjectID: "team"}, true }
	} else {
		f.claims[sid] = claim.Claim{ProjectID: "p1"}
		pol.RequireClaim = true
	}
	r := newRelay(opts)
	go r.Run(t.Context())
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	var attrs []*commonpb.KeyValue
	for _, key := range slices.Concat(rules.PromptFields, rules.PromptDropFields, rules.ToolContentFields) {
		attrs = append(attrs, kv(key, sentinel(key)))
	}
	var resAttrs []*commonpb.KeyValue
	for _, key := range rules.ResourcePromptFields {
		resAttrs = append(resAttrs, kv(key, sentinel(key)))
	}
	recs := []*logspb.LogRecord{{Attributes: append([]*commonpb.KeyValue{kv(sessionKey, sid)}, attrs...)}}
	for _, event := range rules.PromptBodyEvents {
		recs = append(recs, &logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv(sessionKey, sid), kv("event.name", event)},
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sentinel(event)}}})
	}
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{Attributes: resAttrs},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
	sp := &tracepb.Span{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Name: "turn",
		Attributes: append([]*commonpb.KeyValue{kv(sessionKey, sid)}, attrs...)}
	sp.TraceId[0] = 1
	for _, event := range rules.ToolContentEvents {
		sp.Events = append(sp.Events, &tracepb.Span_Event{Name: event})
	}
	traces := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: resAttrs},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}

	for path, msg := range map[string]proto.Message{"/v1/logs": logs, "/v1/traces": traces} {
		body, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if code := post(t, srv, path, body, "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("%s = %d", path, code)
		}
	}
	want := len(recs)
	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["forwarded.logs"] == want && c["forwarded.traces"] == 1
	})
	u.mu.Lock()
	defer u.mu.Unlock()
	var all strings.Builder
	for _, req := range u.requests {
		all.Write(req.body)
	}
	return all.String()
}
