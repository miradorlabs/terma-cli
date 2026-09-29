package relay

import (
	"bytes"
	"context"
	"fmt"
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
	r := New(Options{Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }})
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
	r := New(Options{Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }, Resolve: func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }})
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
	r := New(Options{
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
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		got.Add(1)
	}))
	defer slow.Close()
	f := newFixture()
	r := New(Options{Token: token, Lookup: f.lookup, Grace: 5 * time.Second,
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
		t.Fatalf("host got %d of 30: %v", got.Load(), c)
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
	r := New(Options{Token: token, Lookup: f.lookup,
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
	r := New(Options{Token: token})
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
	r := New(Options{Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(int) (int, bool) { pid := int(sender.Load()); return pid, pid != 0 },
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	r := New(Options{Token: token, Hold: time.Second, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	r := New(Options{Token: token, Resolve: func(claim.Claim) (Policy, error) {
		return Policy{Endpoint: sink.URL, Key: "k", IncludePrompts: false, IncludeToolContent: false}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
