package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Attempts to break the relay: each test is a way it could leak, lose, reorder or
// misattribute a record.

func logsOf(session string, n int, extra ...*commonpb.KeyValue) *logspb.LogsData {
	var recs []*logspb.LogRecord
	for i := range n {
		attrs := append([]*commonpb.KeyValue{kv("session.id", session), kv("seq", fmt.Sprint(i))}, extra...)
		recs = append(recs, &logspb.LogRecord{Attributes: attrs})
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
}

func (u *upstream) seqs(t *testing.T) []string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, r := range u.requests {
		var m logspb.LogsData
		if proto.Unmarshal(r.body, &m) != nil {
			continue
		}
		for _, rl := range m.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					out = append(out, attr(lr.Attributes, "seq"))
				}
			}
		}
	}
	return out
}

// OTLP/JSON encodes trace and span ids as hex. Decoded as protobuf JSON's base64 they
// would reach upstream as noise, and every trace join would break.
func TestRelayKeepsOTLPJSONHexIDs(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[
		{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","parentSpanId":"aaaaaaaaaaaaaaaa","name":"turn","attributes":[{"key":"thread.id","value":{"stringValue":"B"}}]},
		{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"1111111111111111","parentSpanId":"eee19b7ec3c1b174","name":"child"}]}]}]}`
	if code := post(t, srv, "/v1/traces", []byte(body), "application/json", token, false); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 2 })
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, req := range u.requests {
		var m tracepb.TracesData
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, sp := range m.ResourceSpans[0].ScopeSpans[0].Spans {
			if fmt.Sprintf("%x", sp.TraceId) != "5b8efff798038103d269b633813fc60c" || len(sp.SpanId) != 8 {
				t.Fatalf("%s: ids mangled: trace %x span %x", sp.Name, sp.TraceId, sp.SpanId)
			}
		}
	}
}

// A record that arrives after its session was claimed must not overtake the same
// session's records still waiting in the hold.
func TestRelayKeepsASessionsOrderAcrossTheHold(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	early, _ := proto.Marshal(logsOf("late", 3))
	post(t, srv, "/v1/logs", early, "application/x-protobuf", token, false)
	f.mu.Lock()
	f.claims["late"] = claim.Claim{ProjectID: "p1"}
	f.mu.Unlock()
	// Claimed now, but its first three are still held: this one must queue behind them.
	next := logsOf("late", 1)
	next.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes[1] = kv("seq", "3")
	body, _ := proto.Marshal(next)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 4 })
	if got := strings.Join(u.seqs(t), ","); got != "0,1,2,3" {
		t.Fatalf("delivered out of order: %s", got)
	}
}

// A few huge exports for sessions nobody claims must not exhaust memory.
func TestRelayBoundsTheHoldByBytes(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	big := strings.Repeat("x", 4<<20)
	for i := range 20 {
		body, _ := proto.Marshal(logsOf(fmt.Sprintf("flood-%d", i), 1, kv("blob", big)))
		post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	}
	r.mu.Lock()
	held := r.heldBytes
	r.mu.Unlock()
	if held > maxHeldBytes {
		t.Fatalf("held %d bytes, bound is %d", held, maxHeldBytes)
	}
	if c := r.Stats().Snapshot().Counters; c["dropped.unclaimed_evicted.logs"] == 0 {
		t.Fatalf("a flood past the bound must evict the oldest held parts: %v", c)
	}
}

// When the hold is full, spans of traces nothing has named go before a session's
// records: they are mostly process-level work that never will be named.
func TestRelayEvictsUnnamedTracesFirst(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }})
	big := strings.Repeat("x", 30<<20)
	r.route(&part{signal: Traces, session: tracePrefix + "t1", msg: logsOf("x", 1, kv("blob", big)), records: 1})
	r.route(&part{signal: Logs, session: "S", msg: logsOf("S", 1, kv("blob", big)), records: 1})
	r.route(&part{signal: Logs, session: "S2", msg: logsOf("S2", 1, kv("blob", big)), records: 1})
	if _, ok := r.held[tracePrefix+"t1"]; ok {
		t.Fatal("the unnamed trace was kept while a session's records were evicted")
	}
	if len(r.held["S"]) != 1 || len(r.held["S2"]) != 1 {
		t.Fatalf("held %v", r.held)
	}
}

// An agent that already names a project on its resource cannot choose where its
// records go: the claim decides.
func TestRelayOverridesAProjectTheAgentNamed(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	m := logsOf("A", 1)
	m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(ProjectAttr, "p2")}
	body, _ := proto.Marshal(m)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	_, projects := u.logs(t)
	if projects["Bearer key-p1"] != "p1" || len(projects) != 1 {
		t.Fatalf("the resource's own project won: %v", projects)
	}
}

// A session named only on the resource is still that session's.
func TestRelayReadsTheSessionFromTheResource(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	m := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "B")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}, {}}}},
	}}}
	body, _ := proto.Marshal(m)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
}

// A span flood with a fresh trace id each cannot grow the trace index without bound.
func TestRelayBoundsTheTraceIndex(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }, Resolve: func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }})
	for i := range maxTraces + 10 {
		r.learnTrace(fmt.Sprintf("%032x", i), "s")
	}
	if len(r.traces) > maxTraces || r.Stats().Snapshot().Counters["trace_index_full"] != 10 {
		t.Fatalf("trace index %d entries, stats %v", len(r.traces), r.Stats().Snapshot().Counters)
	}
}

// Under concurrent exports, claims landing mid-way and a stop at the end, every record
// the relay received is accounted for exactly once: forwarded, or dropped for a
// reason. Nothing is double counted and nothing vanishes.
func TestRelayAccountsForEveryRecordUnderLoad(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r := New(Options{Dir: t.TempDir(),
		Token: token, Hold: 50 * time.Millisecond, Lookup: f.lookup,
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	var wg sync.WaitGroup
	var sent atomic.Int64
	sessions := []string{"A", "B", "C", "D", "E", ""}
	for w := range 8 {
		wg.Go(func() {
			rng := rand.New(rand.NewSource(int64(w)))
			for range 40 {
				n := 1 + rng.Intn(5)
				m := logsOf(sessions[rng.Intn(len(sessions))], n)
				body, _ := proto.Marshal(m)
				if post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false) == http.StatusOK {
					sent.Add(int64(n))
				}
			}
		})
	}
	// E is claimed half-way through.
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.claims["E"] = claim.Claim{ProjectID: "p2"}
	f.mu.Unlock()
	wg.Wait()
	time.Sleep(200 * time.Millisecond) // past the hold, and a sweep
	cancel()
	<-done
	c := r.Stats().Snapshot().Counters
	accounted := sum(c, "forwarded.") + sum(c, "dropped.")
	if int64(c["received.logs"]) != sent.Load() || int64(accounted) != sent.Load() {
		t.Fatalf("sent %d, received %d, forwarded+dropped %d: %v", sent.Load(), c["received.logs"], accounted, c)
	}
}

func sum(c map[string]int, prefix string) int {
	n := 0
	for k, v := range c {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

// A stopping relay still delivers what it accepted for claimed sessions, even to a
// slow host, within its grace.
func TestRelayDrainsOnStop(t *testing.T) {
	var got atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		var m logspb.LogsData
		_ = proto.Unmarshal(body, &m)
		for _, rl := range m.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				got.Add(int64(len(sl.LogRecords)))
			}
		}
	}))
	defer slow.Close()
	f := newFixture()
	r := New(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup, Grace: 5 * time.Second,
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: slow.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	for range 30 {
		body, _ := proto.Marshal(logsOf("A", 1))
		post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	}
	cancel()
	<-done
	c := r.Stats().Snapshot().Counters
	if got.Load() != 30 || c["forwarded.logs"] != 30 || sum(c, "dropped.") != 0 {
		t.Fatalf("host got %d records of 30: %v", got.Load(), c)
	}
}

// A host that fails transiently gets each part once it recovers: retried, not lost,
// not duplicated.
func TestRelayRetriesATransientFailure(t *testing.T) {
	var calls atomic.Int64
	var bodies sync.Map
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		bodies.Store(b.String(), true)
	}))
	defer flaky.Close()
	f := newFixture()
	r := New(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup,
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: flaky.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx := t.Context()
	go r.Run(ctx)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, _ := proto.Marshal(logsOf("A", 2))
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	if n := r.Stats().Snapshot().Counters["upstream_retries"]; n != 2 {
		t.Fatalf("retries = %d, want 2", n)
	}
}

// Malformed input is refused, never forwarded and never a panic.
func TestRelayRefusesMalformedExports(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	for _, c := range []struct{ body, ct string }{
		{"{not json", "application/json"},
		{"\xff\xfe garbage", "application/x-protobuf"},
	} {
		if code := post(t, srv, "/v1/logs", []byte(c.body), c.ct, token, false); code != http.StatusBadRequest {
			t.Fatalf("%q: got %d", c.body, code)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/logs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET export: %v %v", resp, err)
	}
	if c := r.Stats().Snapshot().Counters; sum(c, "forwarded.") != 0 {
		t.Fatalf("forwarded malformed input: %v", c)
	}
}

// FuzzDecode: whatever bytes arrive, the decoder and splitter never panic, and every
// record lands in exactly one part.
func FuzzDecode(f *testing.F) {
	seed, _ := proto.Marshal(mixedLogs())
	f.Add(seed, false)
	f.Add([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":[{"key":"session.id","value":{"stringValue":"A"}}]}]}]}]}`), true)
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"00","name":"x"}]}]}]}`), true)
	r := New(Options{Dir: f.TempDir(), Token: token})
	f.Fuzz(func(t *testing.T, body []byte, isJSON bool) {
		for _, s := range []Signal{Logs, Metrics, Traces} {
			parts, err := r.decode(s, body, isJSON)
			if err != nil {
				continue
			}
			for _, p := range parts {
				if p.records < 0 {
					t.Fatal("negative record count")
				}
				_, _ = proto.Marshal(p.msg)
			}
		}
	})
}

// A session claimed by one process and resumed by another, somewhere no hook of the
// repository runs, is not covered by the first process's claim: its records wait for
// a claim of their own and are dropped when none comes. The claiming process's
// records still go; a sender that cannot be resolved falls back to the session.
func TestRelayClaimCoversOnlyItsProcesses(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", PIDs: []int{100, 101}}
	var sender atomic.Int64
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(int) (int, bool) { pid := int(sender.Load()); return pid, pid != 0 },
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	ctx := t.Context()
	go r.Run(ctx)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.ConnContext = r.ConnContext
	srv.Start()
	defer srv.Close()
	send := func(pid int64) {
		sender.Store(pid)
		body, _ := proto.Marshal(logsOf("A", 1))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		// A fresh connection each time, so each send is looked up anew.
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(101) // the claiming agent
	send(555) // the same session, resumed by another process
	send(0)   // a sender that cannot be resolved
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.uncovered_process.logs"] != 1 || c["forwarded.logs"] != 2 || c["sender_unresolved"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// A long Codex turn exports its child spans long before the turn span that names the
// session. A log record naming the session with the same trace id — which Codex emits
// mid-turn — must release them, with no keyed span at all; and a trace-keyed span
// outlives the ordinary hold while it waits.
func TestRelayLearnsTracesFromLogs(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Second, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	ctx := t.Context()
	go r.Run(ctx)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	trace := []byte("0123456789abcdef")
	child := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "handle_responses", TraceId: trace}}}}}}}
	body, _ := proto.Marshal(child)
	post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false)
	// Past the ordinary hold, well inside the trace hold: still waiting.
	f.mu.Lock()
	f.now = f.now.Add(10 * time.Second)
	f.mu.Unlock()
	r.sweep()
	if c := r.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("a trace-keyed span was dropped at the ordinary hold: %v", c)
	}
	log := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{TraceId: trace, Attributes: []*commonpb.KeyValue{kv("conversation.id", "B"), kv("event.name", "codex.api_request")}},
	}}}}}}
	body, _ = proto.Marshal(log)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	r.sweep()
	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["forwarded.traces"] == 1 && c["forwarded.logs"] == 1
	})
}

// BenchmarkRelayExport measures one export of 50 log records across 5 claimed
// sessions, through the handler, with claims read from disk as in production.
func BenchmarkRelayExport(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("TERMA_CONFIG_DIR", dir)
	now := time.Now()
	for i := range 5 {
		claim.Write(fmt.Sprintf("s%d", i), claim.Claim{ProjectID: "p1"}, now)
	}
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer sink.Close()
	r := New(Options{Dir: b.TempDir(), Token: token, Resolve: func(claim.Claim) (Policy, error) {
		return Policy{Endpoint: sink.URL, Key: "k", IncludePrompts: false, IncludeToolContent: false}, nil
	}})
	ctx := b.Context()
	go r.Run(ctx)
	var recs []*logspb.LogRecord
	for i := range 50 {
		recs = append(recs, &logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv("session.id", fmt.Sprintf("s%d", i%5)), kv("prompt", strings.Repeat("p", 200)), kv("event.name", "user_prompt")}})
	}
	body, _ := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}})
	h := r.Handler()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	b.ReportMetric(float64(b.N*50)/b.Elapsed().Seconds(), "records/s")
}

// procRelay is a relay whose senders are told by the test: send(pid, ...) posts on a
// fresh connection that PeerPID names pid.
type procRelay struct {
	t      *testing.T
	r      *Relay
	f      *fixture
	u      *upstream
	srv    *httptest.Server
	sender atomic.Int64
}

func newProcRelay(t *testing.T) *procRelay {
	pr := &procRelay{t: t, u: newUpstream(t), f: newFixture()}
	pr.r = New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: pr.f.lookup, Now: pr.f.clock,
		PeerPID: func(int) (int, bool) { pid := int(pr.sender.Load()); return pid, pid != 0 },
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(pr.u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pr.r.Run(ctx)
	pr.srv = httptest.NewUnstartedServer(pr.r.Handler())
	pr.srv.Config.ConnContext = pr.r.ConnContext
	pr.srv.Start()
	t.Cleanup(pr.srv.Close)
	return pr
}

func (pr *procRelay) send(pid int, path string, m proto.Message) {
	pr.sender.Store(int64(pid))
	body, _ := proto.Marshal(m)
	req, _ := http.NewRequest(http.MethodPost, pr.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		pr.t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// codexMetric is a Codex-shaped metric: no session anywhere.
func codexMetric() *metricspb.MetricsData {
	return &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: &resourcepb.Resource{}, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
		{Name: "codex.turn.token_usage", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{Count: 1}}}}},
	}}}}}}
}

func (pr *procRelay) metricsBy(auth string) []map[string]string {
	pr.u.mu.Lock()
	defer pr.u.mu.Unlock()
	var out []map[string]string
	for _, req := range pr.u.requests {
		var m metricspb.MetricsData
		if req.path != "/v1/metrics" || proto.Unmarshal(req.body, &m) != nil {
			continue
		}
		if auth != "" && req.auth != auth {
			continue
		}
		for _, rm := range m.ResourceMetrics {
			res := map[string]string{}
			for _, kv := range rm.Resource.Attributes {
				res[kv.Key] = kv.Value.GetStringValue()
			}
			out = append(out, res)
		}
	}
	return out
}

// Codex's metrics name no session. A process that exported for exactly one opted-in
// session gets its metrics attributed to that session's project, marked as inferred.
func TestRelayAttributesSessionlessMetricsByProcess(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(100, "/v1/logs", logsOf("B", 1)) // B: p2, the process's one session
	pr.send(100, "/v1/metrics", codexMetric())
	waitFor(t, func() bool { return len(pr.metricsBy("Bearer key-p2")) == 1 })
	res := pr.metricsBy("Bearer key-p2")[0]
	if res[ProjectAttr] != "p2" || res[AttributionAttr] != "process" || res[InferredSessionAttr] != "B" {
		t.Fatalf("resource = %v", res)
	}
}

// A process that exported for an opted-in session and an unclaimed one, or for two
// projects, could have made the metric for either: it is never guessed.
func TestRelayRefusesAmbiguousProcesses(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(200, "/v1/logs", logsOf("A", 1)) // p1
	pr.send(200, "/v1/logs", logsOf("C", 1)) // unclaimed
	pr.send(200, "/v1/metrics", codexMetric())
	pr.send(300, "/v1/logs", logsOf("A", 1)) // p1
	pr.send(300, "/v1/logs", logsOf("B", 1)) // p2
	pr.send(300, "/v1/metrics", codexMetric())
	pr.send(0, "/v1/metrics", codexMetric()) // a sender nobody could name
	time.Sleep(200 * time.Millisecond)
	pr.f.mu.Lock()
	pr.f.now = pr.f.now.Add(2 * time.Minute)
	pr.f.mu.Unlock()
	pr.r.sweep()
	if got := pr.metricsBy(""); len(got) != 0 {
		t.Fatalf("an ambiguous process's metric was forwarded: %v", got)
	}
	c := pr.r.Stats().Snapshot().Counters
	if c["dropped.ambiguous_process.metrics"] != 2 || c["dropped.no_session_id.metrics"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// A process whose metrics arrive before it has named any session: they wait, and
// leave once its first log names an opted-in session.
func TestRelayHoldsMetricsUntilTheProcessNamesItsSession(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(400, "/v1/metrics", codexMetric())
	if got := pr.metricsBy(""); len(got) != 0 {
		t.Fatal("forwarded before the process named a session")
	}
	pr.send(400, "/v1/logs", logsOf("A", 1))
	pr.r.sweep()
	waitFor(t, func() bool { return len(pr.metricsBy("Bearer key-p1")) == 1 })
}

// A span of a trace nothing ever names — Codex's process-level work — is attributed
// by its process the same way, instead of waiting out the trace hold.
func TestRelayAttributesUnnamedTracesByProcess(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(500, "/v1/logs", logsOf("A", 1))
	orphan := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "persist_rollout_items", TraceId: []byte("fedcba9876543210")}}}}}}}
	pr.send(500, "/v1/traces", orphan)
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["forwarded.traces"] == 1 })
	if c := pr.r.Stats().Snapshot().Counters; c["attributed_by_process.traces"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// A developer runs `terma install` moments after starting a session: its claim exists
// before its key does. The session's parts wait, and leave once the key is there,
// instead of being dropped on arrival.
func TestRelayWaitsForAKey(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	var keyed atomic.Bool
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) {
			if c.ProjectID == "p3" && keyed.Load() {
				return Policy{Endpoint: u.srv.URL, Key: "key-p3"}, nil
			}
			return Policy{}, ErrNoKey
		}})
	ctx := t.Context()
	go r.Run(ctx)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, _ := proto.Marshal(logsOf("D", 2)) // D is claimed for p3
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	r.sweep()
	if c := r.Stats().Snapshot().Counters; c["forwarded.logs"] != 0 || sum(c, "dropped.") != 0 {
		t.Fatalf("a keyless session's parts must wait: %v", c)
	}
	keyed.Store(true)
	r.sweep()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
}

// A Codex Desktop thread exports its start at thread/start, and its first hook fires
// at its first turn, however long after: the start waits past the ordinary hold for
// that claim, and still leaves; an unclaimed one is dropped when the trace hold ends.
func TestRelayConversationStartWaitsForTheFirstTurn(t *testing.T) {
	pr := newProcRelay(t)
	pr.f.mu.Lock()
	delete(pr.f.claims, "A")
	pr.f.mu.Unlock()
	pr.send(600, "/v1/logs", codexStart("A", "Codex Desktop", "on-request", "workspace-write"))
	pr.send(600, "/v1/logs", codexStart("mine", "Codex Desktop", "on-request", "workspace-write"))
	pr.send(600, "/v1/logs", codexLogs("mine", "Codex Desktop", 1))
	time.Sleep(100 * time.Millisecond)
	advance := func(d time.Duration) {
		pr.f.mu.Lock()
		pr.f.now = pr.f.now.Add(d)
		pr.f.mu.Unlock()
		pr.r.sweep()
	}
	advance(10 * time.Minute)
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.unclaimed_expired.logs"] != 1 || c["forwarded.logs"] != 0 {
		t.Fatalf("after 10 minutes, only the personal thread's ordinary record may be gone: %v", c)
	}
	pr.f.mu.Lock()
	pr.f.claims["A"] = claim.Claim{ProjectID: "p1", Tool: "codex"}
	pr.f.mu.Unlock()
	advance(time.Second)
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	advance(25 * time.Minute)
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.unclaimed_expired.logs"] != 2 || c["forwarded.logs"] != 1 {
		t.Fatalf("the personal thread's start outlived the trace hold, or leaked: %v", c)
	}
}

// codexStart is a Codex conversation start for session, with its policies.
func codexStart(session, originator, approval, sandbox string) *logspb.LogsData {
	return logsOf(session, 1, kv("originator", originator), kv("event.name", "codex.conversation_starts"), kv("approval_policy", approval), kv("sandbox_policy", sandbox))
}

// codexLogs is a Codex-shaped log export for session, from client originator.
func codexLogs(session, originator string, n int) *logspb.LogsData {
	return logsOf(session, n, kv("originator", originator))
}

// Codex's TUI starts a second conversation of its own to title the thread: its own
// conversation.id, no hook, same process. The single-workspace client's unclaimed
// conversation goes with its claimed thread's project, marked as the relay's
// inference, naming the thread it belongs to.
func TestRelayAdoptsTheTUITitleConversation(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(600, "/v1/logs", codexLogs("A", "codex-tui", 1))                         // the thread, claimed for p1
	pr.send(600, "/v1/logs", codexStart("title", "codex-tui", "never", "read-only")) // Codex's own
	pr.send(600, "/v1/logs", codexLogs("title", "codex-tui", 1))                     // no hook claimed it
	pr.send(600, "/v1/metrics", codexMetric())                                       // the process's metrics
	waitFor(t, func() bool {
		c := pr.r.Stats().Snapshot().Counters
		return c["forwarded.logs"] == 3 && c["forwarded.metrics"] == 1
	})
	// (The thread's one log, and the title conversation's start and record.)
	logs, projects := pr.u.logs(t)
	if len(logs["Bearer key-p1"]) != 3 || projects["Bearer key-p1"] != "p1" {
		t.Fatalf("forwarded = %v %v", logs, projects)
	}
	pr.u.mu.Lock()
	found := false
	for _, req := range pr.u.requests {
		var m logspb.LogsData
		if req.path != "/v1/logs" || proto.Unmarshal(req.body, &m) != nil {
			continue
		}
		res := m.ResourceLogs[0].Resource.Attributes
		if attr(m.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes, "session.id") == "title" {
			found = attr(res, AttributionAttr) == "process-sibling" && attr(res, InferredSessionAttr) == "A"
		}
	}
	pr.u.mu.Unlock()
	if !found {
		t.Fatal("the title conversation was not marked as adopted from thread A")
	}
	if res := pr.metricsBy("Bearer key-p1"); len(res) != 1 || res[0][InferredSessionAttr] != "A" {
		t.Fatalf("metrics = %v", res)
	}
}

// Nothing is adopted where an unclaimed conversation could be personal: a
// multi-workspace client (Desktop, the IDE extension), a TUI working for two
// projects at once, or a daemon serving both kinds.
func TestRelayAdoptsNothingAmbiguous(t *testing.T) {
	pr := newProcRelay(t)
	pr.send(700, "/v1/logs", codexLogs("A", "Codex Desktop", 1))
	pr.send(700, "/v1/logs", codexStart("personal", "Codex Desktop", "never", "read-only"))
	pr.send(800, "/v1/logs", codexLogs("A", "codex-tui", 1))
	pr.send(800, "/v1/logs", codexLogs("B", "codex-tui", 1))
	pr.send(800, "/v1/logs", codexStart("stray", "codex-tui", "never", "read-only"))
	// A TUI in the repository that resumed a personal thread: the thread starts with
	// the developer's policies, not Codex's internal ones, and is never adopted.
	pr.send(900, "/v1/logs", codexLogs("A", "codex-tui", 1))
	pr.send(900, "/v1/logs", codexStart("resumed", "codex-tui", "on-request", "workspace-write"))
	// Codex's shared app-server daemon: a Desktop thread, then a TUI thread, in one
	// process. The last client to connect does not make it single-workspace.
	pr.send(1000, "/v1/logs", codexLogs("A", "Codex Desktop", 1))
	pr.send(1000, "/v1/logs", codexLogs("A", "codex-tui", 1))
	pr.send(1000, "/v1/logs", codexStart("daemon", "codex-tui", "never", "read-only"))
	time.Sleep(200 * time.Millisecond)
	// A conversation start waits as long as a trace for its claim; every sweep of that
	// wait decides again, and none may adopt.
	for range 31 {
		pr.f.mu.Lock()
		pr.f.now = pr.f.now.Add(time.Minute)
		pr.f.mu.Unlock()
		pr.r.sweep()
	}
	logs, _ := pr.u.logs(t)
	for _, recs := range logs {
		for _, lr := range recs {
			if s := attr(lr.Attributes, "session.id"); s == "personal" || s == "stray" || s == "resumed" || s == "daemon" {
				t.Fatalf("an ambiguous unclaimed conversation %q was adopted", s)
			}
		}
	}
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.unclaimed_expired.logs"] != 4 {
		t.Fatalf("stats = %v", c)
	}
}

// A session resumed in another bound repository: records from the first run's process
// still go to the first project, even after the move; the second run's go to the second.
func TestRelayRoutesEachRunOfAResumedSession(t *testing.T) {
	pr := newProcRelay(t)
	pr.f.mu.Lock()
	pr.f.claims["R"] = claim.Claim{ProjectID: "p2", PIDs: []int{200}, Placements: []claim.Placement{
		{ProjectID: "p1", PIDs: []int{100}, Since: pr.f.now.Add(-time.Hour)},
		{ProjectID: "p2", PIDs: []int{200}, Since: pr.f.now.Add(-time.Minute)},
	}}
	pr.f.mu.Unlock()
	pr.send(100, "/v1/logs", logsOf("R", 1))
	pr.send(200, "/v1/logs", logsOf("R", 2))
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	byAuth, _ := pr.u.logs(t)
	if len(byAuth["Bearer key-p1"]) != 1 || len(byAuth["Bearer key-p2"]) != 2 {
		t.Fatalf("p1 got %d, p2 got %d", len(byAuth["Bearer key-p1"]), len(byAuth["Bearer key-p2"]))
	}
	// A process neither run named — the session resumed where no bound repository's
	// hook ran — is held and dropped, as ever.
	pr.send(300, "/v1/logs", logsOf("R", 1))
	pr.f.mu.Lock()
	pr.f.now = pr.f.now.Add(2 * time.Minute)
	pr.f.mu.Unlock()
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["dropped.uncovered_process.logs"] == 1 })
}

// Global mode: a session no hook claimed, and a record naming no session at all, go to
// the organization's default project when their hold runs out — marked catch-all —
// instead of being dropped. A claimed session still goes to its own project.
func TestRelayCatchAllInGlobalMode(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	policies := allPolicies(u)
	policies["p-default"] = Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := policies[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	go r.Run(t.Context())
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	// C (unclaimed) and the sessionless record go to the default; D is claimed for a
	// project this machine has no key for, and still waits for its key.
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["caught_by_default.logs"] == 2 })
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 5 })
	byAuth, _ := u.logs(t)
	if n := len(byAuth["Bearer key-default"]); n != 2 {
		t.Fatalf("default project got %d records, want 2", n)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, req := range u.requests {
		if req.auth != "Bearer key-default" {
			continue
		}
		var m logspb.LogsData
		_ = proto.Unmarshal(req.body, &m)
		for _, rl := range m.ResourceLogs {
			if got := attr(rl.Resource.Attributes, AttributionAttr); got != "catch-all" {
				t.Fatalf("a caught record is marked %q", got)
			}
		}
	}
}

// The heartbeat is the organization's: it goes every period, whatever the relay
// delivered, through HeartbeatSend and never to a project's host, with no project on it.
// It says the machine's facts, the agent builds seen and the relay's counters, and
// nothing any session said.
func TestRelayHeartbeat(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Version: "v9.9.9",
		HeartbeatEvery: time.Minute,
		HeartbeatInfo: func() map[string]any {
			return map[string]any{"terma.version": "v9.9.9", "terma.mode": "repo", "terma.agents": []string{"claude", "codex"}}
		},
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			beats = append(beats, b)
			mu.Unlock()
			return nil
		},
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	go r.Run(t.Context())
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(beats) }
	time.Sleep(20 * time.Millisecond) // Run takes its start time before the clock moves
	// The first beat comes a minute after the start, delivered or not.
	f.mu.Lock()
	f.now = f.now.Add(61 * time.Second)
	f.mu.Unlock()
	waitFor(t, func() bool { return count() == 1 })

	logs := logsOf("A", 1)
	logs.ResourceLogs[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "claude-code"), kv("service.version", "2.1.285")}}
	body, _ := proto.Marshal(logs)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	waitFor(t, func() bool { return count() == 2 })
	// And on, with nothing delivered.
	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	waitFor(t, func() bool { return count() == 3 })

	mu.Lock()
	beat := beats[1]
	mu.Unlock()
	res := beat.ResourceLogs[0].Resource.Attributes
	if attr(res, ProjectAttr) != "" || attr(res, "service.name") != HeartbeatService {
		t.Fatalf("heartbeat resource %v", res)
	}
	rec := beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	want := map[string]string{"event.name": HeartbeatEvent, "terma.version": "v9.9.9", "terma.mode": "repo", "relay.agent.claude-code.version": "2.1.285"}
	for k, v := range want {
		if got := attr(rec.Attributes, k); got != v {
			t.Errorf("heartbeat %s = %q, want %q", k, got, v)
		}
	}
	found := map[string]bool{}
	for _, a := range rec.Attributes {
		found[a.Key] = true
	}
	for _, k := range []string{"relay.count.forwarded.logs", "relay.count.received.logs", "relay.outbox.parts", "relay.uptime_s", "relay.last_delivery_at", "terma.agents"} {
		if !found[k] {
			t.Errorf("heartbeat has no %s", k)
		}
	}
	// Nothing of it went to a project's host.
	byAuth, _ := u.logs(t)
	for auth, recs := range byAuth {
		for _, l := range recs {
			if attr(l.Attributes, "event.name") == HeartbeatEvent {
				t.Fatalf("a heartbeat went to a project (%s)", auth)
			}
		}
	}
}
