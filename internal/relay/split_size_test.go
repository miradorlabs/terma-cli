package relay

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// bigTraces is two resources with two scopes each, n spans of about size bytes spread over them.
func bigTraces(n, size int) *tracepb.TracesData {
	td := &tracepb.TracesData{}
	for r := range 2 {
		rs := &tracepb.ResourceSpans{
			Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", fmt.Sprintf("svc-%d", r))}},
			SchemaUrl: "https://opentelemetry.io/schemas/1.26.0",
		}
		for s := range 2 {
			rs.ScopeSpans = append(rs.ScopeSpans, &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: fmt.Sprintf("scope-%d-%d", r, s), Version: "1"}, SchemaUrl: "https://example.test/scope"})
		}
		td.ResourceSpans = append(td.ResourceSpans, rs)
	}
	for i := range n {
		ss := td.ResourceSpans[i%2].ScopeSpans[(i/2)%2]
		ss.Spans = append(ss.Spans, testSpan(i, size))
	}
	return td
}

func testSpan(i, size int) *tracepb.Span {
	return &tracepb.Span{
		TraceId: fmt.Appendf(nil, "trace-%010d", i/50), SpanId: fmt.Appendf(nil, "s%07d", i), ParentSpanId: fmt.Appendf(nil, "s%07d", max(i-1, 0)),
		Name: "span", Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("detail", strings.Repeat("x", size))},
		Events: []*tracepb.Span_Event{{Name: "event", Attributes: []*commonpb.KeyValue{kv("k", "v")}}},
	}
}

// headerTraces is one resource carrying about resourceBytes of attributes and one scope with
// spans of the given detail sizes.
func headerTraces(resourceBytes int, spanSizes ...int) *tracepb.TracesData {
	var spans []*tracepb.Span
	for i, n := range spanSizes {
		spans = append(spans, testSpan(i, n))
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "svc"), kv("huge", strings.Repeat("r", resourceBytes))}},
		SchemaUrl:  "https://opentelemetry.io/schemas/1.26.0",
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "scope"}, Spans: spans}},
	}}}
}

func repeat(n, size int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = size
	}
	return out
}

type placedSpan struct {
	span            *tracepb.Span
	resource, scope proto.Message
	resURL, scURL   string
}

func placedSpans(td *tracepb.TracesData) []placedSpan {
	var out []placedSpan
	for _, rs := range td.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				out = append(out, placedSpan{sp, rs.GetResource(), ss.GetScope(), rs.GetSchemaUrl(), ss.GetSchemaUrl()})
			}
		}
	}
	return out
}

// checkTraceSplit asserts that parts are the export minus the rejected spans: every kept span
// once, in order, unchanged, under its own resource, scope and schema URLs, each part within the
// host's limit, records counted per part, and the parts at most three times the export.
func checkTraceSplit(t *testing.T, td *tracepb.TracesData, parts []*part, rejected int) {
	t.Helper()
	var got []placedSpan
	total := 0
	for i, q := range parts {
		n := proto.Size(q.msg)
		if n > maxRequestBytes {
			t.Errorf("part %d is %d bytes, over the host's %d", i, n, maxRequestBytes)
		}
		total += n
		spans := placedSpans(q.msg.(*tracepb.TracesData))
		if q.records != len(spans) {
			t.Errorf("part %d counts %d records for %d spans", i, q.records, len(spans))
		}
		got = append(got, spans...)
	}
	if total > 3*proto.Size(td) {
		t.Errorf("parts total %d bytes, over three times the %d-byte export", total, proto.Size(td))
	}
	want := placedSpans(td)
	if len(got)+rejected != len(want) {
		t.Fatalf("%d spans kept + %d rejected, want %d", len(got), rejected, len(want))
	}
	j := 0
	for _, g := range got {
		for j < len(want) && !proto.Equal(g.span, want[j].span) {
			j++ // a rejected span
		}
		if j == len(want) {
			t.Fatal("a kept span is missing from the export or out of order")
		}
		w := want[j]
		if !proto.Equal(g.resource, w.resource) || !proto.Equal(g.scope, w.scope) || g.resURL != w.resURL || g.scURL != w.scURL {
			t.Fatalf("span %d moved off its resource, scope or schema URL", j)
		}
		j++
	}
}

// An export over the limit becomes parts that each fit it and that, read in order, are the export:
// every span once, unchanged, under its own resource and scope, the part's other fields kept.
func TestPartWithinSplitsTracesRecordByRecord(t *testing.T) {
	t.Parallel()
	const limit = 64 << 10
	td := bigTraces(600, 900)
	p := &part{signal: Traces, session: "A", msg: td, records: 600, pid: 42, claimed: true}
	parts, rejected := p.within(limit)
	if len(parts) < 2 || rejected != 0 {
		t.Fatalf("a %d-byte export over a %d-byte limit gave %d parts, %d rejected", proto.Size(td), limit, len(parts), rejected)
	}
	for i, q := range parts {
		if n := proto.Size(q.msg); n > limit {
			t.Errorf("part %d is %d bytes, over the %d-byte limit", i, n, limit)
		}
		if q.session != "A" || q.pid != 42 || !q.claimed || q.signal != Traces {
			t.Errorf("part %d lost the part's fields: %+v", i, *q)
		}
	}
	checkTraceSplit(t, td, parts, 0)
}

func TestPartWithinLeavesASmallPartAlone(t *testing.T) {
	t.Parallel()
	p := &part{signal: Traces, msg: bigTraces(4, 10), records: 4}
	if got, rejected := p.within(64 << 10); len(got) != 1 || got[0] != p || rejected != 0 {
		t.Fatalf("a part under the limit was split: %d parts, %d rejected", len(got), rejected)
	}
}

// A span larger than the limit but within the host's goes alone; one larger than the host's limit
// is rejected, and the spans around both are packed as usual.
func TestPartWithinSendsAnOversizedRecordAloneAndRejectsAnUnsendableOne(t *testing.T) {
	t.Parallel()
	const limit = 16 << 10
	td := bigTraces(40, 500)
	spans := td.ResourceSpans[0].ScopeSpans[0].Spans
	spans[3].Attributes = append(spans[3].Attributes, kv("big", strings.Repeat("y", 3*limit)))
	spans[5].Attributes = append(spans[5].Attributes, kv("huge", strings.Repeat("z", maxRequestBytes)))
	parts, rejected := (&part{signal: Traces, msg: td, records: 40}).within(limit)
	if rejected != 1 {
		t.Fatalf("rejected %d spans, want only the one over the host's limit", rejected)
	}
	alone := 0
	for _, q := range parts {
		if proto.Size(q.msg) > limit {
			if q.records != 1 {
				t.Fatalf("an over-limit part holds %d records, want only the oversized one", q.records)
			}
			alone++
		}
	}
	if alone != 1 {
		t.Fatalf("%d over-limit parts, want 1", alone)
	}
	checkTraceSplit(t, td, parts, rejected)
}

// The reviewers' header-heavy exports at the relay's real limits: every span the host could take
// within three times the export's bytes arrives, in parts the host accepts; the rest is rejected.
func TestPartWithinHeaderHeavyExports(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		limit    int
		td       *tracepb.TracesData
		parts    int // 0: any number
		rejected int
	}{
		{"16 KB resource, 400 small spans, 16 KB limit", 16 << 10, headerTraces(16<<10, repeat(400, 60)...), 1, 0},
		{"512 KiB resource, 64 small spans", maxMergeBytes, headerTraces(512<<10, repeat(64, 100)...), 1, 0},
		{"2.25 MiB resource, 3,000 1 KiB spans", maxMergeBytes, headerTraces(2300<<10, repeat(3000, 1000)...), 0, 0},
		{"3.49 MiB resource, 74 3 KB spans", maxMergeBytes, headerTraces(3490<<10, repeat(74, 3000)...), 1, 0},
		{"3.6 MiB resource, 800 small spans", maxMergeBytes, headerTraces(3600<<10, repeat(800, 1000)...), 0, 0},
		{"300 KiB resource, twelve 200 KiB spans", maxMergeBytes, headerTraces(300<<10, repeat(12, 200<<10)...), 0, 0},
		{"4 MiB less 100 KB resource, 500 1 KiB spans", maxMergeBytes, headerTraces(maxRequestBytes-100<<10, repeat(500, 1000)...), 0, 500},
		{"resource over the host's limit", maxMergeBytes, headerTraces(maxRequestBytes, 10, 10), 0, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n := len(placedSpans(c.td))
			parts, rejected := (&part{signal: Traces, msg: c.td, records: n}).within(c.limit)
			if rejected != c.rejected {
				t.Fatalf("rejected %d spans, want %d", rejected, c.rejected)
			}
			if c.parts > 0 && len(parts) != c.parts {
				t.Errorf("%d parts, want %d", len(parts), c.parts)
			}
			checkTraceSplit(t, c.td, parts, rejected)
		})
	}
}

// packRuns on random layouts: every record lands once, in order, in a message or among the
// rejected; no message passes hard; a message passes prefer only for one record alone or a
// header-heavy block; a block's share of the messages stays within three times its size; and a
// record is rejected only when it cannot fit hard beside its header or its header-heavy block
// would pass that bound.
func TestPackRunsProperties(t *testing.T) {
	t.Parallel()
	const prefer, hard = 1000, 8000
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 3000 {
		nb := 1 + rng.IntN(5)
		heads := make([]int, nb)
		sizes := make([][]int, nb)
		for b := range nb {
			heads[b] = []int{1 + rng.IntN(100), 1 + rng.IntN(prefer), 1 + rng.IntN(hard), hard + rng.IntN(100)}[rng.IntN(4)]
			for range rng.IntN(60) {
				sizes[b] = append(sizes[b], []int{1 + rng.IntN(50), 1 + rng.IntN(prefer), 1 + rng.IntN(hard)}[rng.IntN(3)])
			}
		}
		msgs, rejected := packRuns(prefer, hard, heads, sizes)
		seen := map[[2]int]int{}
		var order [][2]int
		share := make([]int, nb)
		for mi, runs := range msgs {
			size, records := 0, 0
			heavy := false
			for _, r := range runs {
				size += heads[r.block]
				share[r.block] += heads[r.block]
				heavy = heavy || 2*heads[r.block] > prefer
				for i := r.from; i < r.to; i++ {
					size += sizes[r.block][i]
					share[r.block] += sizes[r.block][i]
					seen[[2]int{r.block, i}]++
					order = append(order, [2]int{r.block, i})
					records++
				}
			}
			if records == 0 {
				t.Fatalf("trial %d: message %d is empty", trial, mi)
			}
			if size > hard {
				t.Fatalf("trial %d: message %d is %d, over hard %d", trial, mi, size, hard)
			}
			if size > prefer && records > 1 && !heavy {
				t.Fatalf("trial %d: message %d is %d with %d records, over prefer with no heavy header", trial, mi, size, records)
			}
		}
		if !slices.IsSortedFunc(order, func(a, b [2]int) int {
			if a[0] != b[0] {
				return a[0] - b[0]
			}
			return a[1] - b[1]
		}) {
			t.Fatalf("trial %d: records out of order", trial)
		}
		for _, r := range rejected {
			whole := r.from == 0 && r.to == len(sizes[r.block]) && 2*heads[r.block] > prefer
			for i := r.from; i < r.to; i++ {
				seen[[2]int{r.block, i}]++
				if !whole && heads[r.block]+sizes[r.block][i] <= hard {
					t.Fatalf("trial %d: record %d of block %d rejected though it fits hard beside its header", trial, i, r.block)
				}
			}
		}
		for b := range nb {
			own := heads[b]
			for i, n := range sizes[b] {
				if seen[[2]int{b, i}] != 1 {
					t.Fatalf("trial %d: record %d of block %d placed %d times", trial, i, b, seen[[2]int{b, i}])
				}
				own += n
			}
			if share[b] > 3*own {
				t.Fatalf("trial %d: block %d takes %d bytes of messages for %d of its own", trial, b, share[b], own)
			}
		}
	}
}

func TestPartWithinSplitsLogsAndMetrics(t *testing.T) {
	t.Parallel()
	const limit = 32 << 10
	res := func() *resourcepb.Resource {
		return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}}
	}
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: res(), SchemaUrl: "https://example.test/resource",
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "logs"}, SchemaUrl: "https://example.test/scope"}},
	}}}
	for i := range 300 {
		logs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(logs.ResourceLogs[0].ScopeLogs[0].LogRecords,
			&logspb.LogRecord{TraceId: fmt.Appendf(nil, "trace-%010d", i), Attributes: []*commonpb.KeyValue{kv("n", fmt.Sprint(i)), kv("body", strings.Repeat("l", 400))}})
	}
	attrs := func(i int) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{kv("k", strings.Repeat("a", 150)+fmt.Sprint(i))}
	}
	metrics := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: res(), SchemaUrl: "https://example.test/resource",
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "metrics"}, SchemaUrl: "https://example.test/scope"}},
	}}}
	points := 0
	for i := range 250 {
		m := &metricspb.Metric{Name: fmt.Sprintf("m%d", i)}
		switch i % 5 {
		case 0:
			m.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{Attributes: attrs(i)}, {Attributes: attrs(i + 1)}}}}
			points += 2
		case 1:
			m.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{Attributes: attrs(i)}}}}
			points++
		case 2:
			m.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{Attributes: attrs(i)}, {Attributes: attrs(i + 1)}, {Attributes: attrs(i + 2)}}}}
			points += 3
		case 3:
			m.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: []*metricspb.ExponentialHistogramDataPoint{{Attributes: attrs(i)}}}}
			points++
		case 4:
			m.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: []*metricspb.SummaryDataPoint{{Attributes: attrs(i)}, {Attributes: attrs(i + 1)}}}}
			points += 2
		}
		metrics.ResourceMetrics[0].ScopeMetrics[0].Metrics = append(metrics.ResourceMetrics[0].ScopeMetrics[0].Metrics, m)
	}
	type placed struct {
		rec              proto.Message
		res, scope       proto.Message
		resURL, scopeURL string
	}
	flatten := func(m proto.Message) (out []placed) {
		switch d := m.(type) {
		case *logspb.LogsData:
			for _, rl := range d.GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						out = append(out, placed{lr, rl.GetResource(), sl.GetScope(), rl.GetSchemaUrl(), sl.GetSchemaUrl()})
					}
				}
			}
		case *metricspb.MetricsData:
			for _, rm := range d.GetResourceMetrics() {
				for _, sm := range rm.GetScopeMetrics() {
					for _, mt := range sm.GetMetrics() {
						out = append(out, placed{mt, rm.GetResource(), sm.GetScope(), rm.GetSchemaUrl(), sm.GetSchemaUrl()})
					}
				}
			}
		}
		return out
	}
	for _, c := range []struct {
		p       *part
		records int
	}{
		{&part{signal: Logs, msg: logs, records: 300}, 300},
		{&part{signal: Metrics, msg: metrics, records: points}, points},
	} {
		parts, rejected := c.p.within(limit)
		var got []placed
		records := 0
		for _, q := range parts {
			if n := proto.Size(q.msg); n > limit {
				t.Errorf("%s part is %d bytes, over %d", c.p.signal, n, limit)
			}
			records += q.records
			got = append(got, flatten(q.msg)...)
		}
		want := flatten(c.p.msg)
		if len(parts) < 2 || rejected != 0 || records != c.records || len(got) != len(want) {
			t.Fatalf("%s: %d parts, %d rejected, %d records (want %d), %d of %d records", c.p.signal, len(parts), rejected, records, c.records, len(got), len(want))
		}
		for i := range want {
			g, w := got[i], want[i]
			if !proto.Equal(g.rec, w.rec) || !proto.Equal(g.res, w.res) || !proto.Equal(g.scope, w.scope) || g.resURL != w.resURL || g.scopeURL != w.scopeURL {
				t.Fatalf("%s record %d changed or moved off its resource, scope or schema URL", c.p.signal, i)
			}
		}
	}
}

// One large export reaches the host as requests no larger than maxMergeBytes that together carry every span once.
func TestRelaySendsALargeExportInBoundedRequests(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	td := bigTraces(2400, 900) // about 2.5 MiB, under the relay's 16 MiB body limit
	postProto(t, srv, "/v1/traces", td)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 2400 })

	u.mu.Lock()
	defer u.mu.Unlock()
	seen := map[string]int{}
	for _, req := range u.requests {
		if req.path != "/v1/traces" {
			continue
		}
		if len(req.body) > maxMergeBytes {
			t.Errorf("a %d-byte request is over maxMergeBytes (%d)", len(req.body), maxMergeBytes)
		}
		var got tracepb.TracesData
		if err := proto.Unmarshal(req.body, &got); err != nil {
			t.Fatal(err)
		}
		for _, ps := range placedSpans(&got) {
			seen[string(ps.span.GetSpanId())]++
		}
	}
	if len(u.requests) < 2 || len(seen) != 2400 {
		t.Fatalf("%d requests carried %d distinct spans, want several carrying 2400", len(u.requests), len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("span %s sent %d times", id, n)
		}
	}
	if c := r.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("dropped records: %v", c)
	}
}

// strictHost refuses bodies over maxRequestBytes, as the OTLP host does, and records the spans it takes.
type strictHost struct {
	mu      sync.Mutex
	spans   map[string]int
	refused int
}

func newStrictHost(t *testing.T) (*strictHost, *httptest.Server) {
	h := &strictHost{spans: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		var td tracepb.TracesData
		if len(body) > maxRequestBytes || proto.Unmarshal(body, &td) != nil {
			h.refused++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, ps := range placedSpans(&td) {
			h.spans[string(ps.span.GetSpanId())]++
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// A resource too big to share maxMergeBytes with spans still reaches a host that refuses bodies
// over maxRequestBytes: every span arrives once and none is refused.
func TestRelayDeliversAnExportWithAHugeResource(t *testing.T) {
	t.Parallel()
	h, host := newStrictHost(t)
	f := newFixture()
	r, srv := f.relay(t, nil, map[string]Policy{"p1": {Endpoint: host.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}})
	postProto(t, srv, "/v1/traces", headerTraces(2300<<10, repeat(3000, 1000)...)) // 5.3 MB
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 3000 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refused != 0 || len(h.spans) != 3000 {
		t.Fatalf("host refused %d requests and received %d distinct spans, want 0 and 3000", h.refused, len(h.spans))
	}
	for id, n := range h.spans {
		if n != 1 {
			t.Fatalf("span %s arrived %d times", id, n)
		}
	}
	if c := r.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("dropped records: %v", c)
	}
}

// A span no request the host accepts can carry is dropped at enqueue, counted and warned about,
// never sent; the export's other spans are delivered.
func TestRelayDropsASpanTooLargeForTheHost(t *testing.T) {
	t.Parallel()
	h, host := newStrictHost(t)
	f := newFixture()
	r, srv := f.relay(t, nil, map[string]Policy{"p1": {Endpoint: host.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}})
	td := bigTraces(10, 100)
	huge := td.ResourceSpans[1].ScopeSpans[1].Spans[0]
	huge.Attributes = append(huge.Attributes, kv("huge", strings.Repeat("z", maxRequestBytes)))
	postProto(t, srv, "/v1/traces", td)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 9 })
	if c := r.Stats().Snapshot().Counters; c["dropped.too_large.traces"] != 1 || sum(c, "dropped.") != 1 {
		t.Fatalf("counters: %v", c)
	}
	if !slices.ContainsFunc(f.warned(), func(w string) bool { return strings.Contains(w, "cannot fit") }) {
		t.Fatalf("no warning about the dropped span: %v", f.warned())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refused != 0 || len(h.spans) != 9 {
		t.Fatalf("host refused %d requests and received %d spans, want 0 and 9", h.refused, len(h.spans))
	}
}
