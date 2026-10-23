package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// recordingHost refuses bodies over maxRequestBytes, as the OTLP host does, records every body it
// takes, and answers with respond's status when it returns one.
type recordingHost struct {
	mu      sync.Mutex
	bodies  map[Signal][][]byte
	refused int
	respond func(n int, body []byte) int // n counts the bodies taken so far; 0 answers 200
}

func newRecordingHost(t *testing.T) (*recordingHost, *httptest.Server) {
	h := &recordingHost{bodies: map[Signal][][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(body) > maxRequestBytes {
			h.refused++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig := Signal(strings.TrimPrefix(r.URL.Path, "/v1/"))
		n := 0
		for _, b := range h.bodies {
			n += len(b)
		}
		code := 0
		if h.respond != nil {
			code = h.respond(n, body)
		}
		if code == http.StatusBadRequest {
			w.WriteHeader(code)
			return
		}
		h.bodies[sig] = append(h.bodies[sig], body) // a 5xx after taking the body is a lost reply
		if code != 0 {
			w.WriteHeader(code)
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *recordingHost) taken(sig Signal) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.bodies[sig])
}

var claimA = claim.Claim{ProjectID: "p1", Tool: "claude-code"}

// putLegacy writes body to dir's outbox as one entry, as a relay before maxMergeBytes bounded
// entries could have.
func putLegacy(t *testing.T, dir string, sig Signal, msg proto.Message, records int) entry {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	e := newEntry(time.Now(), sig, records)
	if err := (outbox{dir: dir}).put(routeOf(claimA), config.Repository{}, e, body); err != nil {
		t.Fatal(err)
	}
	return e
}

func hostRelay(t *testing.T, dir string, f *fixture, host *httptest.Server, prompts bool) *Relay {
	r := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Grace: 50 * time.Millisecond,
		Warnf: func(format string, args ...any) {
			f.mu.Lock()
			f.warnings = append(f.warnings, fmt.Sprintf(format, args...))
			f.mu.Unlock()
		},
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: host.URL, Key: "k", IncludePrompts: prompts, IncludeToolContent: true}, nil
		}})
	runRelay(t, r)
	return r
}

// identities lists a body's records in order: span ids, log record "n" attributes, metric names.
func identities(t *testing.T, sig Signal, body []byte) []string {
	t.Helper()
	m := newSignalMessage(sig)
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatal(err)
	}
	var out []string
	switch d := m.(type) {
	case *tracepb.TracesData:
		for _, ps := range placedSpans(d) {
			out = append(out, string(ps.span.GetSpanId()))
		}
	case *logspb.LogsData:
		for _, rl := range d.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					out = append(out, attrString(lr.GetAttributes(), "n"))
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range d.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, mt := range sm.GetMetrics() {
					for _, attrs := range pointAttributes(mt) {
						out = append(out, mt.GetName()+"/"+attrString(attrs, "n"))
					}
				}
			}
		}
	}
	return out
}

func pointAttributes(mt *metricspb.Metric) (out [][]*commonpb.KeyValue) {
	switch d := mt.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, p := range d.Gauge.GetDataPoints() {
			out = append(out, p.GetAttributes())
		}
	case *metricspb.Metric_Sum:
		for _, p := range d.Sum.GetDataPoints() {
			out = append(out, p.GetAttributes())
		}
	case *metricspb.Metric_Histogram:
		for _, p := range d.Histogram.GetDataPoints() {
			out = append(out, p.GetAttributes())
		}
	}
	return out
}

// oneMetric is a single gauge of n data points, each carrying size bytes beside its "n".
func oneMetric(n, size int) *metricspb.MetricsData {
	g := &metricspb.Gauge{}
	for i := range n {
		g.DataPoints = append(g.DataPoints, &metricspb.NumberDataPoint{TimeUnixNano: uint64(i + 1), Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(i)},
			Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("n", fmt.Sprint(i)), kv("k", strings.Repeat("p", size))}})
	}
	return &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource:     &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "metrics"}, Metrics: []*metricspb.Metric{{Name: "batched", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: g}}}}},
	}}}
}

func manyLogs(n, size int) *logspb.LogsData {
	ld := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		SchemaUrl: "https://example.test/resource",
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "logs"}}},
	}}}
	for i := range n {
		ld.ResourceLogs[0].ScopeLogs[0].LogRecords = append(ld.ResourceLogs[0].ScopeLogs[0].LogRecords,
			&logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("n", fmt.Sprint(i)), kv("body", strings.Repeat("l", size))}})
	}
	return ld
}

func manyMetrics(n, size int) *metricspb.MetricsData {
	md := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource:     &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "metrics"}}},
	}}}
	for i := range n {
		md.ResourceMetrics[0].ScopeMetrics[0].Metrics = append(md.ResourceMetrics[0].ScopeMetrics[0].Metrics, &metricspb.Metric{
			Name: fmt.Sprintf("m%05d", i),
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
				{Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("k", strings.Repeat("a", size))}},
				{Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("k", strings.Repeat("b", size))}},
			}}},
		})
	}
	return md
}

// checkDelivered asserts the host took every record of want exactly once, in order, in bodies no
// larger than limit, refusing none, and that the outbox drained.
func checkDelivered(t *testing.T, h *recordingHost, dir string, sig Signal, want []string, limit int) {
	t.Helper()
	var got []string
	for i, b := range h.taken(sig) {
		if len(b) > limit {
			t.Errorf("request %d is %d bytes, over %d", i, len(b), limit)
		}
		got = append(got, identities(t, sig, b)...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("host took %d records, want %d, each once and in order", len(got), len(want))
	}
	h.mu.Lock()
	refused := h.refused
	h.mu.Unlock()
	if refused != 0 {
		t.Fatalf("host refused %d requests", refused)
	}
	waitFor(t, func() bool { return pendingParts(t, dir) == 0 })
}

// An entry an older relay wrote over maxMergeBytes, of any signal and even over the host's
// limit, is recovered after a restart and delivered in bounded requests, every record once.
func TestRelaySendsALegacyOversizedEntryInBoundedRequests(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		sig     Signal
		msg     proto.Message
		records int
	}{
		{Traces, bigTraces(6000, 1000), 6000},   // about 6.5 MiB, over the host's limit
		{Logs, manyLogs(6000, 500), 6000},       // about 3.2 MiB
		{Metrics, manyMetrics(4000, 300), 8000}, // 8,000 data points, about 2.6 MiB
		{Metrics, oneMetric(20000, 300), 20000}, // one metric of 20,000 points, about 6.4 MiB
	} {
		t.Run(fmt.Sprintf("%s %d", c.sig, c.records), func(t *testing.T) {
			t.Parallel()
			h, host := newRecordingHost(t)
			dir := t.TempDir()
			body, _ := proto.Marshal(c.msg)
			putLegacy(t, dir, c.sig, c.msg, c.records)
			r := hostRelay(t, dir, newFixture(), host, true)
			waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded."+string(c.sig)] == c.records })
			checkDelivered(t, h, dir, c.sig, identities(t, c.sig, body), maxMergeBytes)
			if m := r.Stats().Snapshot().Counters; m["recovered_from_outbox"] != c.records || sum(m, "dropped.") != 0 {
				t.Fatalf("counters: %v", m)
			}
		})
	}
}

// A legacy entry whose resource is too big to share maxMergeBytes reaches the host in requests
// within its limit; one holding a span no request can carry drops that span once, counted and
// warned about, though the host failed the first attempt, and delivers the rest.
func TestRelaySendsHeaderHeavyAndUnsendableLegacyEntries(t *testing.T) {
	t.Parallel()
	t.Run("header heavy", func(t *testing.T) {
		t.Parallel()
		h, host := newRecordingHost(t)
		dir := t.TempDir()
		td := headerTraces(2300<<10, repeat(3000, 1000)...)
		body, _ := proto.Marshal(td)
		putLegacy(t, dir, Traces, td, 3000)
		r := hostRelay(t, dir, newFixture(), host, true)
		waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] == 3000 })
		checkDelivered(t, h, dir, Traces, identities(t, Traces, body), maxRequestBytes)
	})
	t.Run("unsendable span", func(t *testing.T) {
		t.Parallel()
		h, host := newRecordingHost(t)
		h.respond = func(n int, _ []byte) int {
			if n == 0 {
				return http.StatusServiceUnavailable // the first attempt fails, so the split is made twice
			}
			return 0
		}
		dir := t.TempDir()
		td := bigTraces(10, 100)
		huge := td.ResourceSpans[1].ScopeSpans[1].Spans[0]
		huge.Attributes = append(huge.Attributes, kv("huge", strings.Repeat("z", maxRequestBytes)))
		body, _ := proto.Marshal(td)
		want := slices.DeleteFunc(identities(t, Traces, body), func(id string) bool { return id == string(huge.GetSpanId()) })
		putLegacy(t, dir, Traces, td, 10)
		f := newFixture()
		r := hostRelay(t, dir, f, host, true)
		waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] == 9 })
		bodies := h.taken(Traces)
		var got []string
		for _, b := range bodies[1:] { // the first was taken, then failed: a lost reply
			got = append(got, identities(t, Traces, b)...)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("host took %v after the retry, want %v", got, want)
		}
		if m := r.Stats().Snapshot().Counters; m["dropped.too_large.traces"] != 1 || sum(m, "dropped.") != 1 {
			t.Fatalf("counters: %v", m)
		}
		if !slices.ContainsFunc(f.warned(), func(w string) bool { return strings.Contains(w, "cannot fit") && !strings.Contains(w, "zzz") }) {
			t.Fatalf("no warning about the dropped span, or one quoting it: %v", f.warned())
		}
		waitFor(t, func() bool { return pendingParts(t, dir) == 0 })
	})
}

// Parts queued while prompts were allowed grow when a stricter policy replaces short prompts
// with its marker; what is sent still fits maxMergeBytes, merged parts and a grown single part
// alike, every record arrives once with its prompt withheld, and withholding is counted once a
// record.
func TestRelayBoundsRequestsAStricterPolicyGrew(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := newFixture()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	first := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Grace: 50 * time.Millisecond, Resolve: func(claim.Claim) (Policy, error) {
		return Policy{Endpoint: down.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { first.Run(ctx); close(done) }()
	srv := httptest.NewServer(first.Handler())
	prompts := func(n, from int) *logspb.LogsData {
		ld := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}, ScopeLogs: []*logspb.ScopeLogs{{}}}}}
		for i := range n {
			ld.ResourceLogs[0].ScopeLogs[0].LogRecords = append(ld.ResourceLogs[0].ScopeLogs[0].LogRecords, &logspb.LogRecord{
				Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("event.name", "user_prompt"), kv("prompt", "x"), kv("n", fmt.Sprint(from+i))}})
		}
		return ld
	}
	var want []string
	for i, n := range []int{11000, 4000, 4000} { // one part just under maxMergeBytes, then two to merge
		from := i * 100000
		ld := prompts(n, from)
		for j := range n {
			want = append(want, fmt.Sprint(from+j))
		}
		postProto(t, srv, "/v1/logs", ld)
	}
	srv.Close()
	waitFor(t, func() bool { return first.Stats().Snapshot().Counters["upstream_retries"] >= 1 })
	cancel()
	<-done
	down.Close()

	h, host := newRecordingHost(t)
	second := hostRelay(t, dir, f, host, false)
	waitForCounters(t, second, func(m map[string]int) bool { return m["forwarded.logs"] == len(want) })
	checkDelivered(t, h, dir, Logs, want, maxMergeBytes)
	for _, b := range h.taken(Logs) {
		var ld logspb.LogsData
		_ = proto.Unmarshal(b, &ld)
		for _, lr := range ld.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords() {
			if attrString(lr.GetAttributes(), "prompt") != defaultMarker {
				t.Fatal("a prompt queued before prompts were turned off reached upstream")
			}
		}
	}
	if len(h.taken(Logs)) < 3 {
		t.Fatalf("%d requests: the grown part was not cut, or the grown parts were merged", len(h.taken(Logs)))
	}
	if m := second.Stats().Snapshot().Counters; m["withheld_at_send_records"] != len(want) || sum(m, "dropped.") != 0 {
		t.Fatalf("counters: %v", m)
	}
}

// Across a cut part, a lost reply resends only the piece in flight, a failed request sends the
// same piece again, and a refused piece is dropped, counted and set aside alone: the rest of the
// part is delivered once, in order.
func TestRelayRetriesAndRefusalsWithinACutPart(t *testing.T) {
	t.Parallel()
	td := bigTraces(4000, 1000) // about 4.3 MiB: nine pieces
	body, _ := proto.Marshal(td)
	ids := identities(t, Traces, body)
	for _, c := range []struct {
		name    string
		respond func(n int, body []byte) int
		check   func(t *testing.T, h *recordingHost, r *Relay)
	}{
		{"lost reply", func(n int, _ []byte) int {
			if n == 2 {
				return http.StatusServiceUnavailable // taken, then the reply is lost
			}
			return 0
		}, func(t *testing.T, h *recordingHost, r *Relay) {
			bodies := h.taken(Traces)
			if !slices.Equal(identities(t, Traces, bodies[2]), identities(t, Traces, bodies[3])) {
				t.Fatal("the piece whose reply was lost was not the one sent again")
			}
			var got []string
			for i, b := range bodies {
				if i != 2 {
					got = append(got, identities(t, Traces, b)...)
				}
			}
			if !slices.Equal(got, ids) {
				t.Fatal("beside the resent piece, records are missing, repeated or out of order")
			}
		}},
		{"refused piece", func() func(int, []byte) int {
			var once sync.Once
			return func(n int, _ []byte) int {
				code := 0
				if n == 3 {
					once.Do(func() { code = http.StatusBadRequest })
				}
				return code
			}
		}(), func(t *testing.T, h *recordingHost, r *Relay) {
			var got []string
			for _, b := range h.taken(Traces) {
				got = append(got, identities(t, Traces, b)...)
			}
			m := r.Stats().Snapshot().Counters
			refused := m["dropped.upstream_refused_400.traces"]
			if refused == 0 || len(got)+refused != len(ids) || m["forwarded.traces"] != len(got) {
				t.Fatalf("delivered %d and refused %d of %d: %v", len(got), refused, len(ids), m)
			}
			var j int
			for _, id := range got {
				for j < len(ids) && ids[j] != id {
					j++
				}
				if j == len(ids) {
					t.Fatal("a delivered record is out of order or repeated")
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, host := newRecordingHost(t)
			h.respond = c.respond
			dir := t.TempDir()
			putLegacy(t, dir, Traces, td, 4000)
			r := hostRelay(t, dir, newFixture(), host, true)
			waitFor(t, func() bool { return pendingParts(t, dir) == 0 })
			for _, b := range h.taken(Traces) {
				if len(b) > maxMergeBytes {
					t.Fatalf("a %d-byte request", len(b))
				}
			}
			c.check(t, h, r)
			if c.name == "refused piece" {
				des, _ := os.ReadDir(filepath.Join(dir, deadDir))
				if len(des) != 1 || !strings.Contains(des[0].Name(), ".000") {
					t.Fatalf("dead directory holds %v, want the refused piece alone", des)
				}
			}
		})
	}
}

// A crash while a part is cut leaves it whole, its pieces set aside and removed; one after the
// cut, with the first piece delivered, leaves the rest, which goes merged with the part after
// it. Either way every record arrives once, in order, and only what is left is counted.
func TestRelayResumesACutPartAfterACrash(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		committed bool // the part was removed, and its first piece delivered
	}{{"mid-cut", false}, {"after the first piece", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			td := bigTraces(700, 1000) // about 760 KiB: two pieces
			stored, _ := proto.Marshal(td)
			e := putLegacy(t, dir, Traces, td, 700)
			p, err := planCut(Traces, stored, stored, maxMergeBytes)
			if err != nil || len(p.bodies) != 2 {
				t.Fatalf("plan: %d pieces, %v", len(p.bodies), err)
			}
			ob, rt := outbox{dir: dir}, routeOf(claimA)
			want := identities(t, Traces, stored)
			if c.committed {
				pieces, err := ob.split(rt, e, p.bodies, p.records)
				if err != nil {
					t.Fatal(err)
				}
				ob.remove(rt, pieces[:1])
				want = identities(t, Traces, p.bodies[1])
			} else {
				ts, seq := e.stem()
				// Only the first piece was written when the relay stopped.
				name := fmt.Sprintf("%s-%s.%06d-%s-%d%s", ts, seq, 0, Traces, p.records[0], pbSuffix)
				if err := config.WriteFileAtomicNoSync(filepath.Join(ob.routeDir(rt), name), p.bodies[0], 0o600); err != nil {
					t.Fatal(err)
				}
			}
			next := bigTraces(20, 100)
			for _, rs := range next.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						sp.SpanId = append([]byte("n"), sp.SpanId...)
					}
				}
			}
			putLegacy(t, dir, Traces, next, 20)
			nextBody, _ := proto.Marshal(next)
			want = append(want, identities(t, Traces, nextBody)...)

			h, host := newRecordingHost(t)
			r := hostRelay(t, dir, newFixture(), host, true)
			waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] >= len(want) })
			checkDelivered(t, h, dir, Traces, want, maxMergeBytes)
			if c.committed && len(h.taken(Traces)) != 1 {
				t.Fatalf("%d requests, want the rest merged with the next part", len(h.taken(Traces)))
			}
			if m := r.Stats().Snapshot().Counters; m["recovered_from_outbox"] != len(want) || m["forwarded.traces"] != len(want) || sum(m, "dropped.") != 0 {
				t.Fatalf("counters: %v, want %d recovered and forwarded", m, len(want))
			}
		})
	}
}

// Pieces of a cut part the janitor drops while the first is in flight stay dropped once it is
// delivered: nothing of them is written back, sent or counted again.
func TestRelayKeepsACutPartTheJanitorDropped(t *testing.T) {
	t.Parallel()
	h, host := newRecordingHost(t)
	inFlight, release := make(chan struct{}), make(chan struct{})
	h.respond = func(n int, _ []byte) int {
		if n == 0 {
			close(inFlight)
			<-release
		}
		return 0
	}
	dir := t.TempDir()
	putLegacy(t, dir, Traces, bigTraces(2000, 1000), 2000)
	r := hostRelay(t, dir, newFixture(), host, true)
	<-inFlight
	if removed := r.sweepOutbox(time.Now().Add(maxOutboxAge + time.Hour)); len(removed[routeOf(claimA)]) < 2 {
		t.Fatalf("the janitor removed %v", removed)
	}
	// As a part put meanwhile would, recreate the route directory the janitor removed with it.
	if err := os.MkdirAll((outbox{dir: dir}).routeDir(routeOf(claimA)), 0o700); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] > 0 })
	time.Sleep(200 * time.Millisecond)
	if n := len(h.taken(Traces)); n != 1 || pendingParts(t, dir) != 0 {
		t.Fatalf("%d requests and %d parts left after the drop, want the piece in flight alone and none", n, pendingParts(t, dir))
	}
	if m := r.Stats().Snapshot().Counters; m["dropped.outbox_expired.traces"] != 2000 || sum(m, "dropped.") != 2000 {
		t.Fatalf("counters: %v", m)
	}
}

// A part the janitor dropped between being read and being cut is not cut: no piece of it is
// written, and its queue stays as the janitor left it.
func TestCutLeavesAPartTheJanitorDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	td := bigTraces(700, 1000)
	stored, _ := proto.Marshal(td)
	e := putLegacy(t, dir, Traces, td, 700)
	r := newRelay(Options{Dir: dir, Token: token})
	s := &sender{r: r, route: routeOf(claimA), wake: make(chan struct{}, 1), queue: map[Signal][]entry{}}
	p, err := planCut(Traces, stored, stored, maxMergeBytes)
	if err != nil {
		t.Fatal(err)
	}
	s.push(e)
	if removed := r.sweepOutbox(time.Now().Add(maxOutboxAge + time.Hour)); len(removed[s.route]) != 1 {
		t.Fatalf("the janitor removed %v", removed)
	}
	s.forget([]entry{e}) // as sweepOutbox does for a sender it knows
	if err := os.MkdirAll((outbox{dir: dir}).routeDir(s.route), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.cut(e, p); err == nil {
		t.Fatal("a dropped part was cut")
	}
	if n := pendingParts(t, dir); n != 0 || len(s.next(maxMergeFiles)) != 0 {
		t.Fatalf("%d parts on disk after cutting a dropped part", n)
	}
}

// A record whose request alone encodes to exactly the host's limit is sent, though the framing
// the packer assumes puts it over: only the exact size rejects a record.
func TestRelaySendsARecordOfExactlyTheHostLimit(t *testing.T) {
	t.Parallel()
	td := bigTraces(10, 100)
	edge := td.ResourceSpans[1].ScopeSpans[1].Spans[0]
	edge.Attributes = append(edge.Attributes, kv("edge", ""))
	pad := edge.Attributes[len(edge.Attributes)-1].Value.GetValue().(*commonpb.AnyValue_StringValue)
	n := maxRequestBytes - 200
	for range 5 {
		pad.StringValue = strings.Repeat("e", n)
		n += maxRequestBytes - layoutOfTraces(td).alone(3, 0)
	}
	if l := layoutOfTraces(td); l.alone(3, 0) != maxRequestBytes || l.heads[3]+l.sizes[3][0] <= maxRequestBytes {
		t.Fatalf("the record's request is %d, its estimate %d", l.alone(3, 0), l.heads[3]+l.sizes[3][0])
	}
	body, _ := proto.Marshal(td)
	h, host := newRecordingHost(t)
	dir := t.TempDir()
	putLegacy(t, dir, Traces, td, 10)
	r := hostRelay(t, dir, newFixture(), host, true)
	waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] == 10 })
	checkDelivered(t, h, dir, Traces, identities(t, Traces, body), maxRequestBytes)
	if !slices.ContainsFunc(h.taken(Traces), func(b []byte) bool { return len(b) == maxRequestBytes }) {
		t.Fatal("no request carried the record alone at the host's limit")
	}
	if m := r.Stats().Snapshot().Counters; sum(m, "dropped.") != 0 {
		t.Fatalf("counters: %v", m)
	}
}

func layoutOfTraces(td *tracepb.TracesData) layout {
	l, _ := layoutOf(td)
	return l
}

// pendingParts counts the parts left in dir's outbox, not those set aside in its dead directory.
func pendingParts(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == deadDir {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), pbSuffix) && !strings.HasPrefix(d.Name(), ".tmp-") {
			n++
		}
		return nil
	})
	return n
}

// A legacy entry whose resource takes most of the host's limit reaches the host whole, though
// every request repeats the resource: each request is within the host's limit, every span arrives
// once and in order, and none is dropped.
func TestRelaySendsEveryRecordUnderAResourceNearTheHostLimit(t *testing.T) {
	t.Parallel()
	h, host := newRecordingHost(t)
	dir := t.TempDir()
	td := headerTraces(maxRequestBytes-100<<10, repeat(500, 1000)...)
	body, _ := proto.Marshal(td)
	putLegacy(t, dir, Traces, td, 500)
	r := hostRelay(t, dir, newFixture(), host, true)
	waitForCounters(t, r, func(m map[string]int) bool { return m["forwarded.traces"] == 500 })
	checkDelivered(t, h, dir, Traces, identities(t, Traces, body), maxRequestBytes)
	if m := r.Stats().Snapshot().Counters; sum(m, "dropped.") != 0 {
		t.Fatalf("counters: %v", m)
	}
	sent := 0
	for _, b := range h.taken(Traces) {
		sent += len(b)
	}
	t.Logf("%d requests, %d bytes for a %d-byte entry", len(h.taken(Traces)), sent, len(body))
}

// A relay that stopped while cutting a part leaves the part and the pieces it wrote. When the part
// is past its age, or the outbox over its size, the next relay drops the part and its pieces
// together: no piece of it is sent, and only the part is counted.
func TestRelayDropsAnInterruptedCutWithItsPart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, why string
		evicted   bool
	}{{"expired", "outbox_expired", false}, {"evicted", "outbox_full", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			td := bigTraces(700, 1000)
			stored, _ := proto.Marshal(td)
			e := putLegacy(t, dir, Traces, td, 700)
			p, err := planCut(Traces, stored, stored, maxMergeBytes)
			if err != nil || len(p.bodies) != 2 {
				t.Fatalf("plan: %d pieces, %v", len(p.bodies), err)
			}
			ob, rt := outbox{dir: dir}, routeOf(claimA)
			ts, seq := e.stem()
			piece := fmt.Sprintf("%s-%s.%06d-%s-%d%s", ts, seq, 0, Traces, p.records[0], pbSuffix)
			if err := config.WriteFileAtomicNoSync(filepath.Join(ob.routeDir(rt), piece), p.bodies[0], 0o600); err != nil {
				t.Fatal(err)
			}
			dropped := 700
			if c.evicted {
				// A newer part as large as the outbox's bound; its size, not its bytes, matters.
				filler := newEntry(time.Now().Add(time.Second), Traces, 1)
				f, err := os.Create(filepath.Join(ob.routeDir(rt), filler.name))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(maxOutboxBytes + 1); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
				dropped++
			} else {
				past := time.Now().Add(-maxOutboxAge - time.Hour)
				_ = os.Chtimes(filepath.Join(ob.routeDir(rt), e.name), past, past)
			}
			h, host := newRecordingHost(t)
			r := hostRelay(t, dir, newFixture(), host, true)
			waitForCounters(t, r, func(m map[string]int) bool { return m["dropped."+c.why+".traces"] == dropped })
			time.Sleep(300 * time.Millisecond)
			if n := len(h.taken(Traces)); n != 0 || pendingParts(t, dir) != 0 {
				t.Fatalf("%d requests and %d parts left: a piece of the dropped part survived", n, pendingParts(t, dir))
			}
			if m := r.Stats().Snapshot().Counters; m["forwarded.traces"] != 0 || sum(m, "dropped.") != dropped {
				t.Fatalf("counters: %v", m)
			}
		})
	}
}

// A part of a signal the policy no longer allows is dropped as it is, never cut: an oversized
// legacy entry of a disabled signal leaves no pieces and sends nothing.
func TestRelayDropsADisabledSignalsPartWithoutCuttingIt(t *testing.T) {
	t.Parallel()
	h, host := newRecordingHost(t)
	dir := t.TempDir()
	putLegacy(t, dir, Traces, bigTraces(2000, 1000), 2000)
	var pieces atomic.Int32
	r := newRelay(Options{Dir: dir, Token: token, Lookup: newFixture().lookup, Grace: 50 * time.Millisecond,
		Resolve: func(claim.Claim) (Policy, error) {
			_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err == nil && strings.Contains(d.Name(), ".000") {
					pieces.Add(1)
				}
				return nil
			})
			return Policy{Endpoint: host.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true, Signals: []string{"logs"}}, nil
		}})
	runRelay(t, r)
	waitForCounters(t, r, func(m map[string]int) bool { return m["dropped.policy_signal_or_content.traces"] == 2000 })
	waitFor(t, func() bool { return pendingParts(t, dir) == 0 })
	if n := len(h.taken(Traces)); n != 0 || pieces.Load() != 0 {
		t.Fatalf("%d requests, %d pieces seen: the part was cut or sent", n, pieces.Load())
	}
}
