package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Each test here is a way the relay could leak, lose, reorder or misattribute a record.

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

// OTLP/JSON's hex trace and span ids reach upstream intact, not decoded as base64 noise.
func TestRelayKeepsOTLPJSONHexIDs(t *testing.T) {
	t.Parallel()
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

// A record arriving after its session's claim does not overtake that session's held records.
func TestRelayKeepsASessionsOrderAcrossTheHold(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	early, _ := proto.Marshal(logsOf("late", 3))
	post(t, srv, "/v1/logs", early, "application/x-protobuf", token, false)
	f.mu.Lock()
	f.claims["late"] = claim.Claim{ProjectID: "p1"}
	f.mu.Unlock()
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
	t.Parallel()
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

// A full hold evicts spans of unnamed traces before a session's records.
func TestRelayEvictsUnnamedTracesFirst(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }})
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

// A project an agent names on its resource does not choose where its records go: the claim does.
func TestRelayOverridesAProjectTheAgentNamed(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	m := logsOf("A", 1)
	m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(semconv.MiradorProjectIDKey, "p2")}
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
	t.Parallel()
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
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }, Resolve: func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }})
	for i := range maxTraces + 10 {
		r.learnTrace(fmt.Sprintf("%032x", i), "s", false)
	}
	if len(r.traces) > maxTraces || r.Stats().Snapshot().Counters["trace_index_full"] != 10 {
		t.Fatalf("trace index %d entries, stats %v", len(r.traces), r.Stats().Snapshot().Counters)
	}
}

// Under concurrent exports, mid-way claims and a stop, every record is accounted for exactly once.
func TestRelayAccountsForEveryRecordUnderLoad(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r := newRelay(Options{Dir: t.TempDir(),
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

// A stopping relay still delivers what it accepted, even to a slow host, within its grace.
func TestRelayDrainsOnStop(t *testing.T) {
	t.Parallel()
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
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup, Grace: 5 * time.Second,
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

// A host that fails transiently gets each part exactly once after it recovers.
func TestRelayRetriesATransientFailure(t *testing.T) {
	t.Parallel()
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
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup,
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: flaky.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	runRelay(t, r)
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
	t.Parallel()
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
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET export: %v %v", resp, err)
	}
	if c := r.Stats().Snapshot().Counters; sum(c, "forwarded.") != 0 {
		t.Fatalf("forwarded malformed input: %v", c)
	}
}

// FuzzDecode checks that the decoder and splitter never panic and every record lands in one part.
func FuzzDecode(f *testing.F) {
	seed, _ := proto.Marshal(mixedLogs())
	f.Add(seed, false)
	f.Add([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":[{"key":"session.id","value":{"stringValue":"A"}}]}]}]}]}`), true)
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"00","name":"x"}]}]}]}`), true)
	r := newRelay(Options{Dir: f.TempDir(), Token: token})
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

// A session resumed by another process where no hook runs is not covered by the first
// process's claim; the claiming process's records still go.
func TestRelayClaimCoversOnlyItsProcesses(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", PIDs: []int{100, 101}}
	var sender atomic.Int64
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(int) (int, bool) { pid := int(sender.Load()); return pid, pid != 0 },
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	runRelay(t, r)
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
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(101) // the claiming agent
	send(555) // the same session, resumed by another process
	send(0)   // a sender that cannot be resolved: it may be the resumed one too
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.uncovered_process.logs"] != 2 || c["forwarded.logs"] != 1 || c["sender_unresolved"] == 0 {
		t.Fatalf("stats = %v", c)
	}
}

// What a stopping relay still holds is dropped under the reason that held it, so a claimed
// session's loss names the claim's gap rather than "unclaimed".
func TestRelayExitNamesWhyHeld(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", PIDs: []int{100}}
	var logged []string
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(int) (int, bool) { return 555, true },
		Logf:    func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	srv, stop := startRelay(t, r)
	postProto(t, srv, "/v1/logs", logsOf("A", 2))
	postProto(t, srv, "/v1/logs", logsOf("Z", 1))
	r.sweep()
	stop()
	c := r.Stats().Snapshot().Counters
	if c["dropped.uncovered_process_at_exit.logs"] != 2 || c["dropped.unclaimed_expired_at_exit.logs"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	if len(logged) != 2 || !strings.Contains(strings.Join(logged, "\n"), "uncovered_process_at_exit A: 2 logs from pid 555") {
		t.Fatalf("log = %q", logged)
	}
}

// A mid-turn log record naming the session and trace releases the turn's held child spans,
// which outlive the ordinary hold while they wait.
func TestRelayLearnsTracesFromLogs(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Second, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	runRelay(t, r)
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

// BenchmarkRelayExport measures one 50-record export across 5 claimed sessions, claims read from disk.
func BenchmarkRelayExport(b *testing.B) {
	dir := b.TempDir()
	now := time.Now()
	for i := range 5 {
		claim.Write(dir, fmt.Sprintf("s%d", i), claim.Claim{ProjectID: "p1"}, now)
	}
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer sink.Close()
	r := newRelay(Options{Dir: b.TempDir(), Token: token,
		Lookup: func(id string, now time.Time) (claim.Claim, bool) { return claim.Read(dir, id, now) },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: sink.URL, Key: "k", IncludePrompts: false, IncludeToolContent: false}, nil
		}})
	runRelay(b, r)
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

// procRelay is a relay whose senders the test names: send(pid, ...) posts as pid.
type procRelay struct {
	t      *testing.T
	r      *Relay
	f      *fixture
	u      *upstream
	srv    *httptest.Server
	sender atomic.Int64
	dead   sync.Map // pid → true once the test says the process exited
}

func newProcRelay(t *testing.T) *procRelay {
	pr := &procRelay{t: t, u: newUpstream(t), f: newFixture()}
	pr.r = newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: pr.f.lookup, Now: pr.f.clock,
		PeerPID:      func(int) (int, bool) { pid := int(pr.sender.Load()); return pid, pid != 0 },
		ProcessAlive: func(pid int) bool { _, gone := pr.dead.Load(pid); return !gone },
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(pr.u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, pr.r)
	pr.srv = httptest.NewUnstartedServer(pr.r.Handler())
	pr.srv.Config.ConnContext = pr.r.ConnContext
	pr.srv.Start()
	t.Cleanup(pr.srv.Close)
	return pr
}

func (pr *procRelay) advance(d time.Duration) {
	pr.f.mu.Lock()
	pr.f.now = pr.f.now.Add(d)
	pr.f.mu.Unlock()
	pr.r.sweep()
}

func (pr *procRelay) exit(pid int) {
	pr.dead.Store(pid, true)
	pr.r.sweep() // seen gone
	pr.advance(exitGrace + time.Second)
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

// codexMetric is a metric that names no session anywhere.
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

// Sessionless metrics wait while their process runs, then go, marked inferred, to the one
// claimed session it named.
func TestRelayAttributesSessionlessMetricsOnceTheProcessExits(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(100, "/v1/logs", logsOf("B", 1)) // B: p2, the process's one session
	pr.send(100, "/v1/metrics", codexMetric())
	time.Sleep(100 * time.Millisecond)
	pr.advance(time.Minute)
	if got := pr.metricsBy(""); len(got) != 0 {
		t.Fatalf("a running process's metric was attributed: %v", got)
	}
	pr.exit(100)
	waitFor(t, func() bool { return len(pr.metricsBy("Bearer key-p2")) == 1 })
	res := pr.metricsBy("Bearer key-p2")[0]
	if res[semconv.MiradorProjectIDKey] != "p2" || res[semconv.TermaRelayAttributionKey] != "process" || res[semconv.TermaRelaySessionIDKey] != "B" {
		t.Fatalf("resource = %v", res)
	}
}

// A process that named several sessions, or never exits within the hold, has its metrics
// dropped, never guessed.
func TestRelayRefusesAmbiguousProcesses(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(200, "/v1/logs", logsOf("A", 1)) // p1
	pr.send(200, "/v1/logs", logsOf("C", 1)) // unclaimed
	pr.send(200, "/v1/metrics", codexMetric())
	pr.send(300, "/v1/logs", logsOf("A", 1)) // p1
	pr.send(300, "/v1/logs", logsOf("B", 1)) // p2
	pr.send(300, "/v1/metrics", codexMetric())
	pr.send(301, "/v1/logs", logsOf("A", 1)) // p1, and still running
	pr.send(301, "/v1/metrics", codexMetric())
	pr.send(0, "/v1/metrics", codexMetric()) // a sender nobody could name
	time.Sleep(200 * time.Millisecond)
	pr.dead.Store(200, true)
	pr.dead.Store(300, true)
	pr.r.sweep()
	pr.advance(31 * time.Minute) // past the hold
	if got := pr.metricsBy(""); len(got) != 0 {
		t.Fatalf("an ambiguous process's metric was forwarded: %v", got)
	}
	c := pr.r.Stats().Snapshot().Counters
	if c["dropped.ambiguous_process.metrics"] != 2 || c["dropped.process_running.metrics"] != 1 || c["dropped.no_session_id.metrics"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// Metrics that arrive before their process names a session wait until it names one and exits.
func TestRelayHoldsMetricsUntilTheProcessNamesItsSession(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(400, "/v1/metrics", codexMetric())
	if got := pr.metricsBy(""); len(got) != 0 {
		t.Fatal("forwarded before the process named a session")
	}
	pr.send(400, "/v1/logs", logsOf("A", 1))
	pr.exit(400)
	waitFor(t, func() bool { return len(pr.metricsBy("Bearer key-p1")) == 1 })
}

// A span of a trace nothing names goes with its process's one session once the process exits.
func TestRelayAttributesUnnamedTracesOnceTheProcessExits(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(500, "/v1/logs", logsOf("A", 1))
	orphan := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "persist_rollout_items", TraceId: []byte("fedcba9876543210")}}}}}}}
	pr.send(500, "/v1/traces", orphan)
	time.Sleep(100 * time.Millisecond)
	if c := pr.r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 {
		t.Fatalf("an unnamed span left while its process ran: %v", c)
	}
	pr.exit(500)
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["forwarded.traces"] == 1 })
	if c := pr.r.Stats().Snapshot().Counters; c["attributed_by_process.traces"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// A shared process that has shown one claimed session does not send an unnamed trace's
// spans to that project: they wait for the trace and follow it.
func TestRelayNeverAttributesAnUnnamedTraceOfARunningSharedProcess(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(600, "/v1/logs", codexLogs("A", "Codex Desktop", 1)) // claimed, p1
	trace := []byte("personal-trace-1")
	early := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "handle_responses", TraceId: trace, SpanId: []byte("span0001")},
	}}}}}}
	pr.send(600, "/v1/traces", early)
	time.Sleep(100 * time.Millisecond)
	pr.advance(time.Second)
	if c := pr.r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 {
		t.Fatalf("an unnamed span of a shared process left for the claimed session's project: %v", c)
	}
	named := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "session_task.turn", TraceId: trace, SpanId: []byte("span0002"), Attributes: []*commonpb.KeyValue{kv("thread.id", "personal-0000-4000-8000-000000000001")}},
	}}}}}}
	pr.send(600, "/v1/traces", named)
	pr.advance(31 * time.Minute)
	if c := pr.r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 || c["dropped.unclaimed_expired.traces"] != 2 {
		t.Fatalf("the personal thread's spans: %v", c)
	}
}

// A claim that lands before its key holds the session's parts until the key is there.
func TestRelayWaitsForAKey(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	var keyed atomic.Bool
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) {
			if c.ProjectID == "p3" && keyed.Load() {
				return Policy{Endpoint: u.srv.URL, Key: "key-p3"}, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, r)
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

// A conversation start waits past the ordinary hold for its first turn's claim; an
// unclaimed one is dropped when the trace hold ends.
func TestRelayConversationStartWaitsForTheFirstTurn(t *testing.T) {
	t.Parallel()
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

func codexStart(session, originator, approval, sandbox string) *logspb.LogsData {
	return logsOf(session, 1, kv("originator", originator), kv("event.name", "codex.conversation_starts"), kv("approval_policy", approval), kv("sandbox_policy", sandbox))
}

func codexLogs(session, originator string, n int) *logspb.LogsData {
	return logsOf(session, n, kv("originator", originator))
}

// An unclaimed conversation is never adopted, whatever its policies, client or neighbours,
// and its process, having named two sessions, has its metrics dropped.
func TestRelayAdoptsNothing(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	pr.send(700, "/v1/logs", codexLogs("A", "codex-tui", 1))                            // claimed, p1
	pr.send(700, "/v1/logs", codexStart("personal", "codex-tui", "never", "read-only")) // looks like the title generator
	pr.send(700, "/v1/logs", codexLogs("personal", "codex-tui", 1))
	pr.send(700, "/v1/metrics", codexMetric())
	time.Sleep(200 * time.Millisecond)
	pr.exit(700)
	pr.advance(31 * time.Minute)
	logs, _ := pr.u.logs(t)
	for _, recs := range logs {
		for _, lr := range recs {
			if attr(lr.Attributes, "session.id") == "personal" {
				t.Fatal("an unclaimed conversation was adopted")
			}
		}
	}
	c := pr.r.Stats().Snapshot().Counters
	if c["forwarded.logs"] != 1 || c["dropped.unclaimed_expired.logs"] != 2 || c["dropped.ambiguous_process.metrics"] != 1 {
		t.Fatalf("stats = %v", c)
	}
}

// A session resumed in another bound repository: records from the first run's process
// still go to the first project, even after the move; the second run's go to the second.
func TestRelayRoutesEachRunOfAResumedSession(t *testing.T) {
	t.Parallel()
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
	// A process neither run named is held and dropped.
	pr.send(300, "/v1/logs", logsOf("R", 1))
	pr.f.mu.Lock()
	pr.f.now = pr.f.now.Add(2 * time.Minute)
	pr.f.mu.Unlock()
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["dropped.uncovered_process.logs"] == 1 })
}

// In global mode unclaimed and sessionless records go to the team's project at once; a
// session whose hook claims every one (B, Codex's) goes by that claim instead.
func TestRelayCatchAllInGlobalMode(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	policies := allPolicies(u)
	policies["p-default"] = Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := policies[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 6 })
	if c := r.Stats().Snapshot().Counters; c["attributed_by_catch_all.logs"] != 4 || sum(c, "attributed_by_process.") != 0 {
		t.Fatalf("catch-all deliveries counted as %v", c)
	}
	byAuth, _ := u.logs(t)
	if n, b := len(byAuth["Bearer key-default"]), len(byAuth["Bearer key-p2"]); n != 4 || b != 2 {
		t.Fatalf("default project got %d records, B's claim %d, want 4 and 2", n, b)
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
			if got := attr(rl.Resource.Attributes, semconv.TermaRelayAttributionKey); got != "catch-all" {
				t.Fatalf("a caught record is marked %q", got)
			}
		}
	}
}

// A stopping relay waits for a heartbeat still in flight, so its outcome is counted
// before Run returns.
func TestRelayStopWaitsForAHeartbeatInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture()
	sending := make(chan struct{})
	var finished atomic.Bool
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup, Now: f.clock, Grace: time.Second,
		HeartbeatEvery: time.Minute,
		HeartbeatSend: func(ctx context.Context, _ *logspb.LogsData) error {
			close(sending)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // a sender slow to give up
			finished.Store(true)
			return ctx.Err()
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	// Move the clock until a beat starts, however late Run takes its start time.
	for beating := false; !beating; {
		f.mu.Lock()
		f.now = f.now.Add(61 * time.Second)
		f.mu.Unlock()
		select {
		case <-sending:
			beating = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if !finished.Load() || r.Stats().Snapshot().Counters["heartbeats_failed"] != 1 {
		t.Fatalf("Run returned before its heartbeat: finished %v, counters %v", finished.Load(), r.Stats().Snapshot().Counters)
	}
}

// The heartbeat goes every period through HeartbeatSend, never the outbox, and names
// terma's version, the machine and the reason, timestamped when sent.
func TestRelayHeartbeat(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Version: "v9.9.9",
		HeartbeatEvery: time.Minute,
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
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(beats) }
	time.Sleep(20 * time.Millisecond) // Run takes its start time before the clock moves
	// The first beat comes a minute after the start, delivered or not.
	f.mu.Lock()
	f.now = f.now.Add(61 * time.Second)
	f.mu.Unlock()
	waitFor(t, func() bool { return count() == 1 })
	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	waitFor(t, func() bool { return count() == 2 })

	// Asked for one (as `terma setup` does), it beats now, with the reason given.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/heartbeat?reason=setup", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /heartbeat: %v %v", resp, err)
	}
	if count() != 3 {
		t.Fatalf("a requested beat was not sent: %d beats", count())
	}
	mu.Lock()
	defer mu.Unlock()
	reasons := []string{}
	for _, b := range beats {
		reasons = append(reasons, attr(b.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes, semconv.TermaRelayHeartbeatReasonKey))
	}
	if strings.Join(reasons, ",") != "start,interval,setup" {
		t.Errorf("beat reasons %v", reasons)
	}
	beat := beats[2]
	if res := beat.ResourceLogs[0].Resource.Attributes; attr(res, semconv.ServiceNameKey) != HeartbeatService || attr(res, semconv.ServiceVersionKey) != "v9.9.9" ||
		attr(res, semconv.TermaSchemaVersionKey) != semconv.SchemaVersion {
		t.Fatalf("heartbeat resource %v", res)
	}
	host, _ := os.Hostname()
	rec := beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if rec.EventName != semconv.TermaRelayHeartbeatEvent {
		t.Errorf("heartbeat EventName = %q", rec.EventName)
	}
	if got := attr(rec.Attributes, semconv.HostNameKey); got != host {
		t.Errorf("heartbeat host.name = %q, want %q", got, host)
	}
	// The requested beat carries its reason, host and counters, never an exit reason. Each
	// beat's send is counted after it left, so this one reports the interval beat's.
	for _, kv := range rec.Attributes[2:] {
		if !strings.HasPrefix(kv.Key, semconv.TermaRelayHeartbeatCounterKey+".") {
			t.Errorf("the requested beat carries %s", kv.Key)
		}
	}
	if got := beatCounters(rec)["heartbeats_sent"]; got != 1 {
		t.Errorf("the requested beat reports %d beats sent since the last, want 1", got)
	}
	if rec.TimeUnixNano != uint64(f.clock().UnixNano()) {
		t.Errorf("heartbeat time %d, want the send time", rec.TimeUnixNano)
	}
}

// A failed sender lookup is retried at the connection's next export, not kept for its life.
func TestRelayRetriesAFailedSenderLookup(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", PIDs: []int{101}}
	var lookups atomic.Int32
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(int) (int, bool) {
			if lookups.Add(1) == 1 {
				return 0, false
			}
			return 101, true
		},
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	runRelay(t, r)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.ConnContext = r.ConnContext
	srv.Start()
	defer srv.Close()
	client := &http.Client{} // keep-alive: both exports on one connection
	for range 2 {
		body, _ := proto.Marshal(logsOf("A", 1))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// The first export's unknown sender is dropped, never a widened claim; the second is covered.
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] >= 1 })
	if n := lookups.Load(); n != 2 {
		t.Fatalf("looked up %d times, want a retry after the failure", n)
	}
}
