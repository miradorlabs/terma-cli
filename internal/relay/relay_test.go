package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
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

// fixture: A (p1) and B (p2) are claimed with keys, C is unclaimed, D is claimed but keyless.
type fixture struct {
	mu       sync.Mutex
	claims   map[string]claim.Claim
	now      time.Time
	warnings []string
}

func (f *fixture) warned() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.warnings)
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
	r := newRelay(Options{Dir: t.TempDir(),
		Token:  token,
		Hold:   time.Minute,
		Lookup: f.lookup,
		Now:    f.clock,
		Warnf: func(format string, args ...any) {
			f.mu.Lock()
			f.warnings = append(f.warnings, fmt.Sprintf(format, args...))
			f.mu.Unlock()
		},
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

// Each session's records go to its own project with its own key, and nothing crosses over.
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
				if v := attr(lr.Attributes, "prompt"); v != "" && v != "[REDACTED]" {
					t.Fatalf("p2 prompt leaked: %q", v)
				}
				if attr(lr.Attributes, "arguments") != "" || attr(lr.Attributes, "output") != "" {
					t.Fatalf("p2 tool content leaked: %v", lr.Attributes)
				}
			}
			c := r.Stats().Snapshot().Counters
			// C and D wait: a claim or a key may still come.
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

// An unclaimed session's records are released by a claim inside the hold, else dropped.
func TestRelayHoldReleasesOrExpires(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })

	f.mu.Lock()
	f.now = f.now.Add(30 * time.Second)
	f.claims["C"] = claim.Claim{ProjectID: "p1"}
	f.mu.Unlock()
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 4 })

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

// One metric carrying several sessions' points reaches each project with only its own.
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
		// Metrics naming no session are dropped.
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

// A span naming no session belongs to its trace's: at once when the trace is known, after a
// hold when the child arrives first.
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
	later := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "session_task.turn", TraceId: early, Attributes: []*commonpb.KeyValue{kv("thread.id", "B")}},
	}}}}}}
	body, _ = proto.Marshal(later)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 5 })
}

// A root span exported last that names thread_id releases its trace's spans; a numeric
// thread_id is an OS thread, never a session.
func TestRelayNamesTracesBySessionLoop(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	loop, other := []byte("1111111111111111"), []byte("2222222222222222")
	work := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "turn_context.build", TraceId: loop, Attributes: []*commonpb.KeyValue{kv("thread.id", "12")}},
		{Name: "persist_rollout_items", TraceId: loop},
		{Name: "list_models", TraceId: other, Attributes: []*commonpb.KeyValue{kv("thread_id", "8")}},
	}}}}}}
	body, _ := proto.Marshal(work)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	r.sweep()
	if c := r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 {
		t.Fatalf("spans of an unnamed trace left before it was named: %v", c)
	}
	end := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "session_loop", TraceId: loop, Attributes: []*commonpb.KeyValue{kv("thread_id", "B"), kv("thread.id", "8")}},
	}}}}}}
	body, _ = proto.Marshal(end)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 3 })
	if c := r.Stats().Snapshot().Counters; c["held_parts"] < 1 {
		t.Fatalf("stats = %v", c)
	}
}

// Only exporters holding the local token can inject telemetry into a project.
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

// A token leading the path is accepted exactly, never as a prefix of another segment.
func TestRelayAcceptsTheTokenInThePath(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	for path, want := range map[string]int{
		"/" + token + "/v1/logs":                http.StatusOK,
		"/" + token + "x/v1/logs":               http.StatusUnauthorized,
		"/wrong/v1/logs":                        http.StatusNotFound,
		"/" + token[:len(token)-1] + "/v1/logs": http.StatusNotFound,
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// A refusal may be a 401 or a 404 (no such route): any 4xx is one.
		refused := resp.StatusCode/100 == 4
		if want == http.StatusOK && resp.StatusCode != want || want != http.StatusOK && !refused {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	if c := r.Stats().Snapshot().Counters; c["received.logs"] == 0 {
		t.Fatalf("the path-token export was not received: %v", c)
	}
}

// A body a host refuses for good (400) is set aside in .dead/ and never retried.
func TestRelayUpstreamRefusalIsFinal(t *testing.T) {
	u := newUpstream(t)
	u.status = http.StatusBadRequest
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["dropped.upstream_refused_400.logs"] == 3 })
	// Session C is still held, waiting for a claim: the relay is not idle.
	if _, ok := r.Idle(); ok {
		t.Fatal("a relay holding records is not idle")
	}
	if c := r.Stats().Snapshot().Counters; c["upstream_retries"] != 0 {
		t.Fatalf("a refusal was retried: %v", c)
	}
	if n := countFiles(t, filepath.Join(r.opts.Dir, deadDir)); n == 0 {
		t.Fatal("a refused part was not set aside in .dead/")
	}
	// Refused records are lost for good, so the relay log says so without debug on.
	if w := f.warned(); !slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "refused") }) {
		t.Fatalf("no warning for the refusal: %q", w)
	}
}

// A part refused for its key (401, 403) stays queued and is retried slowly, never dropped.
func TestRelayRefusedKeyIsRetriedNotDropped(t *testing.T) {
	u := newUpstream(t)
	u.status = http.StatusForbidden
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["upstream_retries"] >= 2 })
	c := r.Stats().Snapshot().Counters
	if sum(c, "dropped.upstream_") != 0 || sum(c, "forwarded.") != 0 {
		t.Fatalf("a refused key dropped or delivered: %v", c)
	}
	if n := countFiles(t, r.opts.Dir); n != 2 {
		t.Fatalf("outbox holds %d parts, want the two projects' 2", n)
	}
}
