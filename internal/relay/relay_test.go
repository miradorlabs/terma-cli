package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const token = "local-token"

// upstream records what reached each project's host, by key.
type upstream struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []upstreamRequest
	status   int
}

type upstreamRequest struct {
	path, auth string
	body       []byte
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{status: http.StatusOK}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, upstreamRequest{r.URL.Path, r.Header.Get("Authorization"), body})
		status := u.status
		u.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) logs(t *testing.T) (byAuth map[string][]*logspb.LogRecord, projects map[string]string) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	byAuth, projects = map[string][]*logspb.LogRecord{}, map[string]string{}
	for _, r := range u.requests {
		if r.path != "/v1/logs" {
			continue
		}
		var m logspb.LogsData
		if err := proto.Unmarshal(r.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.ResourceLogs {
			projects[r.auth] = attr(rl.Resource.Attributes, ProjectAttr)
			for _, sl := range rl.ScopeLogs {
				byAuth[r.auth] = append(byAuth[r.auth], sl.LogRecords...)
			}
		}
	}
	return byAuth, projects
}

func attr(kvs []*commonpb.KeyValue, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: strValue(v)}
}

// fixture: sessions A (project p1) and B (p2) are claimed with keys, C is unclaimed,
// D is claimed for a project this machine has no key for.
type fixture struct {
	mu     sync.Mutex
	claims map[string]claim.Claim
	now    time.Time
}

func newFixture() *fixture {
	return &fixture{now: time.Unix(1_800_000_000, 0), claims: map[string]claim.Claim{
		"A": {ProjectID: "p1", Tool: "claude-code"},
		"B": {ProjectID: "p2", Tool: "codex"},
		"D": {ProjectID: "p3"},
	}}
}

func (f *fixture) lookup(s string, _ time.Time) (claim.Claim, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.claims[s]
	return c, ok
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) relay(t *testing.T, u *upstream, policies map[string]Policy) (*Relay, *httptest.Server) {
	r := New(Options{
		Token:  token,
		Hold:   time.Minute,
		Lookup: f.lookup,
		Now:    f.clock,
		Resolve: func(c claim.Claim) (Policy, error) {
			p, ok := policies[c.ProjectID]
			if !ok {
				return Policy{}, ErrNoKey
			}
			return p, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	// Give Run its context before the first destination starts.
	time.Sleep(10 * time.Millisecond)
	return r, srv
}

func allPolicies(u *upstream) map[string]Policy {
	return map[string]Policy{
		"p1": {Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true},
		"p2": {Endpoint: u.srv.URL, Key: "key-p2", IncludePrompts: false, IncludeToolContent: false},
	}
}

func mixedLogs() *logspb.LogsData {
	rec := func(key, sid, event string, extra ...*commonpb.KeyValue) *logspb.LogRecord {
		attrs := []*commonpb.KeyValue{kv("event.name", event)}
		if sid != "" {
			attrs = append(attrs, kv(key, sid))
		}
		return &logspb.LogRecord{Attributes: append(attrs, extra...)}
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			rec("session.id", "A", "user_prompt", kv("prompt", "secret A")),
			rec("conversation.id", "B", "codex.user_prompt", kv("prompt", "secret B")),
			rec("conversation.id", "B", "codex.tool_result", kv("arguments", "{}"), kv("output", "ok"), kv("call_id", "c1")),
			rec("session.id", "C", "user_prompt", kv("prompt", "personal")),
			rec("session.id", "D", "user_prompt", kv("prompt", "not opted in")),
			rec("", "", "no_session"),
		}}},
	}}}
}

func post(t *testing.T, srv *httptest.Server, path string, body []byte, contentType, auth string, gz bool) int {
	t.Helper()
	if gz {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = b.Bytes()
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Each session's records go to its own project with its own key; the unclaimed one
// waits, the keyless and the unattributable ones are dropped; nothing crosses over.
func TestRelayRoutesLogsPerClaimedSession(t *testing.T) {
	for _, enc := range []struct {
		name, contentType string
		marshal           func(proto.Message) ([]byte, error)
		gzip              bool
	}{
		{"protobuf", "application/x-protobuf", proto.Marshal, false},
		{"json", "application/json", protojson.Marshal, false},
		{"protobuf-gzip", "application/x-protobuf", proto.Marshal, true},
	} {
		t.Run(enc.name, func(t *testing.T) {
			u := newUpstream(t)
			f := newFixture()
			r, srv := f.relay(t, u, allPolicies(u))
			body, err := enc.marshal(mixedLogs())
			if err != nil {
				t.Fatal(err)
			}
			if code := post(t, srv, "/v1/logs", body, enc.contentType, token, enc.gzip); code != http.StatusOK {
				t.Fatalf("export = %d", code)
			}
			waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
			got, projects := u.logs(t)
			if len(got["Bearer key-p1"]) != 1 || len(got["Bearer key-p2"]) != 2 || len(got) != 2 {
				t.Fatalf("forwarded by key = %v", got)
			}
			if projects["Bearer key-p1"] != "p1" || projects["Bearer key-p2"] != "p2" {
				t.Fatalf("project stamp = %v", projects)
			}
			// p1 lets prompts through; p2 withholds prompts and tool content.
			if v := attr(got["Bearer key-p1"][0].Attributes, "prompt"); v != "secret A" {
				t.Fatalf("p1 prompt = %q", v)
			}
			for _, lr := range got["Bearer key-p2"] {
				if v := attr(lr.Attributes, "prompt"); v != "" && v != codexRedacted {
					t.Fatalf("p2 prompt leaked: %q", v)
				}
				if attr(lr.Attributes, "arguments") != "" || attr(lr.Attributes, "output") != "" {
					t.Fatalf("p2 tool content leaked: %v", lr.Attributes)
				}
			}
			c := r.Stats().Snapshot().Counters
			// C (unclaimed) and D (keyless) wait: a claim or a key may still come.
			if c["received.logs"] != 6 || c["dropped.no_session_id.logs"] != 1 || c["held_parts"] != 2 {
				t.Fatalf("stats = %v", c)
			}
			f.mu.Lock()
			f.now = f.now.Add(2 * time.Minute)
			f.mu.Unlock()
			r.sweep()
			c = r.Stats().Snapshot().Counters
			if c["dropped.no_key.logs"] != 1 || c["dropped.unclaimed_expired.logs"] != 1 {
				t.Fatalf("after the hold: %v", c)
			}
		})
	}
}

// An unclaimed session's records are released if the claim arrives inside the hold
// and dropped, never sent, if it does not.
func TestRelayHoldReleasesOrExpires(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })

	// C is claimed for p1 30 seconds later: released.
	f.mu.Lock()
	f.now = f.now.Add(30 * time.Second)
	f.claims["C"] = claim.Claim{ProjectID: "p1"}
	f.mu.Unlock()
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 4 })

	// A new unclaimed session E, never claimed: dropped once the hold is over.
	e := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("session.id", "E"), kv("prompt", "personal")}}}}}}}}
	body, _ = proto.Marshal(e)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.unclaimed_expired.logs"] != 1 || c["forwarded.logs"] != 4 {
		t.Fatalf("stats = %v", c)
	}
	if idle, ok := r.Idle(); !ok || idle < 0 {
		t.Fatalf("a relay holding nothing is idle: %v %v", idle, ok)
	}
}

// Metric data points are split per session: one metric can carry several sessions'
// points, and each project gets a copy with only its own.
func TestRelaySplitsMetricPoints(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	point := func(sid string, v int64) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{Attributes: []*commonpb.KeyValue{kv("session.id", sid)}, Value: &metricspb.NumberDataPoint_AsInt{AsInt: v}}
	}
	m := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
		{Name: "claude_code.token.usage", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints: []*metricspb.NumberDataPoint{point("A", 10), point("B", 20), point("C", 30)}}}},
		// Codex's metrics name no session: dropped.
		{Name: "codex.turn.token_usage", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{Count: 1}}}}},
	}}}}}}
	body, _ := proto.Marshal(m)
	post(t, srv, "/v1/metrics", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.metrics"] == 2 })
	c := r.Stats().Snapshot().Counters
	if c["received.metrics"] != 4 || c["dropped.no_session_id.metrics"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, req := range u.requests {
		var got metricspb.MetricsData
		if err := proto.Unmarshal(req.body, &got); err != nil {
			t.Fatal(err)
		}
		sum := got.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum()
		if len(sum.DataPoints) != 1 || !sum.IsMonotonic || sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
			t.Fatalf("%s got %v", req.auth, sum)
		}
		want := map[string]int64{"Bearer key-p1": 10, "Bearer key-p2": 20}[req.auth]
		if sum.DataPoints[0].GetAsInt() != want {
			t.Fatalf("%s got point %d, want %d", req.auth, sum.DataPoints[0].GetAsInt(), want)
		}
	}
}

// Spans split the same way, and Codex's spans name their session as thread.id — on
// the turn span only. Its children inherit the trace, so a span naming no session
// belongs to its trace's: at once when the trace is known, after a hold when the child
// arrives first.
func TestRelaySplitsSpans(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	turn, early := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	tr := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "claude_code.tool", Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("full_command", "ls")}},
		{Name: "session_task.turn", TraceId: turn, Attributes: []*commonpb.KeyValue{kv("thread.id", "B")}},
		{Name: "handle_responses", TraceId: turn},
		{Name: "tool.child", TraceId: early},
		{Name: "orphan"},
	}}}}}}
	body, _ := proto.Marshal(tr)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 3 })
	if c := r.Stats().Snapshot().Counters; c["dropped.no_session_id.traces"] != 1 || c["held_parts"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	// The early child's trace is named by a keyed span in a later export.
	later := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "session_task.turn", TraceId: early, Attributes: []*commonpb.KeyValue{kv("thread.id", "B")}},
	}}}}}}
	body, _ = proto.Marshal(later)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 5 })
}

// Only the agents' exporters, holding the local token, can send; nothing else on the
// machine can inject telemetry into a project.
func TestRelayRefusesWithoutToken(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	for _, auth := range []string{"", "wrong"} {
		if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", auth, false); code != http.StatusUnauthorized {
			t.Fatalf("auth %q got %d", auth, code)
		}
	}
	if c := r.Stats().Snapshot().Counters; c["received.logs"] != 0 || c["refused_unauthorized"] != 2 {
		t.Fatalf("stats = %v", c)
	}
}

// A host that refuses the key is not asked again for that record; one that fails is
// retried.
func TestRelayUpstreamRefusalIsFinal(t *testing.T) {
	u := newUpstream(t)
	u.status = http.StatusForbidden
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["dropped.upstream_refused_403.logs"] == 3 })
	// Session C is still held, waiting for a claim: the relay is not idle.
	if _, ok := r.Idle(); ok {
		t.Fatal("a relay holding records is not idle")
	}
	if c := r.Stats().Snapshot().Counters; c["upstream_retries"] != 0 {
		t.Fatalf("a refusal was retried: %v", c)
	}
}
