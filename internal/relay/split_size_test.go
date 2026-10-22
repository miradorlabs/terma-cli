package relay

import (
	"fmt"
	"io"
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
			rs.ScopeSpans = append(rs.ScopeSpans, &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: fmt.Sprintf("scope-%d-%d", r, s), Version: "1"}})
		}
		td.ResourceSpans = append(td.ResourceSpans, rs)
	}
	for i := range n {
		ss := td.ResourceSpans[i%2].ScopeSpans[(i/2)%2]
		ss.Spans = append(ss.Spans, &tracepb.Span{
			TraceId: []byte(fmt.Sprintf("trace-%010d", i/50)), SpanId: []byte(fmt.Sprintf("s%07d", i)), ParentSpanId: []byte(fmt.Sprintf("s%07d", max(i-1, 0))),
			Name: "span", Attributes: []*commonpb.KeyValue{kv("session.id", "A"), kv("detail", strings.Repeat("x", size))},
		})
	}
	return td
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

// An export over the limit becomes parts that each fit it and that, read in order, are the export:
// every span once, unchanged, under its own resource and scope, the part's other fields kept.
func TestPartWithinSplitsTracesRecordByRecord(t *testing.T) {
	t.Parallel()
	const limit = 64 << 10
	td := bigTraces(600, 900)
	p := &part{signal: Traces, session: "A", msg: td, records: 600, pid: 42, claimed: true}
	parts := p.within(limit)
	if len(parts) < 2 {
		t.Fatalf("a %d-byte export over a %d-byte limit stayed in %d part", proto.Size(td), limit, len(parts))
	}
	var got []placedSpan
	records := 0
	for i, q := range parts {
		if n := proto.Size(q.msg); n > limit {
			t.Errorf("part %d is %d bytes, over the %d-byte limit", i, n, limit)
		}
		spans := placedSpans(q.msg.(*tracepb.TracesData))
		if q.records != len(spans) {
			t.Errorf("part %d counts %d records for %d spans", i, q.records, len(spans))
		}
		if q.session != "A" || q.pid != 42 || !q.claimed || q.signal != Traces {
			t.Errorf("part %d lost the part's fields: %+v", i, *q)
		}
		records += q.records
		got = append(got, spans...)
	}
	want := placedSpans(td)
	if records != 600 || len(got) != len(want) {
		t.Fatalf("parts carry %d records, %d spans; want 600", records, len(got))
	}
	for i := range want {
		g, w := got[i], want[i]
		if !proto.Equal(g.span, w.span) || !proto.Equal(g.resource, w.resource) || !proto.Equal(g.scope, w.scope) || g.resURL != w.resURL || g.scURL != w.scURL {
			t.Fatalf("span %d changed or moved off its resource or scope", i)
		}
	}
}

func TestPartWithinLeavesASmallPartAlone(t *testing.T) {
	t.Parallel()
	p := &part{signal: Traces, msg: bigTraces(4, 10), records: 4}
	if got := p.within(64 << 10); len(got) != 1 || got[0] != p {
		t.Fatalf("a part under the limit was split: %d parts", len(got))
	}
}

// A span bigger than the limit cannot be split further: it travels alone, the spans around it packed as usual.
func TestPartWithinSendsAnOversizedRecordAlone(t *testing.T) {
	t.Parallel()
	const limit = 16 << 10
	td := bigTraces(40, 500)
	td.ResourceSpans[0].ScopeSpans[0].Spans[3].Attributes = append(td.ResourceSpans[0].ScopeSpans[0].Spans[3].Attributes, kv("huge", strings.Repeat("y", 3*limit)))
	parts := (&part{signal: Traces, msg: td, records: 40}).within(limit)
	alone, total := 0, 0
	for _, q := range parts {
		total += q.records
		if proto.Size(q.msg) > limit {
			if q.records != 1 {
				t.Fatalf("an over-limit part holds %d records, want only the oversized one", q.records)
			}
			alone++
		}
	}
	if alone != 1 || total != 40 {
		t.Fatalf("%d over-limit parts, %d records; want 1 and 40", alone, total)
	}
}

func TestPartWithinSplitsLogsAndMetrics(t *testing.T) {
	t.Parallel()
	const limit = 32 << 10
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "logs"}}},
	}}}
	for i := range 300 {
		logs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(logs.ResourceLogs[0].ScopeLogs[0].LogRecords,
			&logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv("n", fmt.Sprint(i)), kv("body", strings.Repeat("l", 400))}})
	}
	metrics := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource:     &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "metrics"}}},
	}}}
	for i := range 200 {
		pts := []*metricspb.NumberDataPoint{
			{Attributes: []*commonpb.KeyValue{kv("k", strings.Repeat("a", 200))}, Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(i)}},
			{Attributes: []*commonpb.KeyValue{kv("k", strings.Repeat("b", 200))}, Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(i)}},
		}
		metrics.ResourceMetrics[0].ScopeMetrics[0].Metrics = append(metrics.ResourceMetrics[0].ScopeMetrics[0].Metrics,
			&metricspb.Metric{Name: fmt.Sprintf("m%d", i), Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: pts}}})
	}
	for _, c := range []struct {
		p       *part
		records int
		names   func(proto.Message) []string
	}{
		{&part{signal: Logs, msg: logs, records: 300}, 300, func(m proto.Message) (out []string) {
			for _, rl := range m.(*logspb.LogsData).GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						out = append(out, rl.GetResource().GetAttributes()[0].GetValue().GetStringValue()+"/"+sl.GetScope().GetName()+"/"+lr.GetAttributes()[0].GetValue().GetStringValue())
					}
				}
			}
			return out
		}},
		{&part{signal: Metrics, msg: metrics, records: 400}, 400, func(m proto.Message) (out []string) {
			for _, rm := range m.(*metricspb.MetricsData).GetResourceMetrics() {
				for _, sm := range rm.GetScopeMetrics() {
					for _, mt := range sm.GetMetrics() {
						out = append(out, rm.GetResource().GetAttributes()[0].GetValue().GetStringValue()+"/"+sm.GetScope().GetName()+"/"+mt.GetName())
					}
				}
			}
			return out
		}},
	} {
		parts := c.p.within(limit)
		var got []string
		records := 0
		for _, q := range parts {
			if n := proto.Size(q.msg); n > limit {
				t.Errorf("%s part is %d bytes, over %d", c.p.signal, n, limit)
			}
			records += q.records
			got = append(got, c.names(q.msg)...)
		}
		if len(parts) < 2 || records != c.records || !slices.Equal(got, c.names(c.p.msg)) {
			t.Errorf("%s: %d parts, %d records (want %d), records in order: %v", c.p.signal, len(parts), records, c.records, slices.Equal(got, c.names(c.p.msg)))
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

// A resource too big to share the limit with records is not copied once per record: with small
// spans the split stays under twice the export, and every record still arrives once, in order.
func TestPartWithinBoundsAnOversizedHeader(t *testing.T) {
	t.Parallel()
	const limit = 16 << 10
	td := bigTraces(400, 60)
	for _, rs := range td.ResourceSpans {
		rs.Resource.Attributes = append(rs.Resource.Attributes, kv("huge", strings.Repeat("r", limit)))
	}
	parts := (&part{signal: Traces, msg: td, records: 400}).within(limit)
	var got []placedSpan
	total := 0
	for _, q := range parts {
		total += proto.Size(q.msg)
		got = append(got, placedSpans(q.msg.(*tracepb.TracesData))...)
	}
	if total > 2*proto.Size(td) {
		t.Fatalf("splitting a %d-byte export wrote %d bytes in %d parts, over twice the export", proto.Size(td), total, len(parts))
	}
	want := placedSpans(td)
	if len(got) != len(want) {
		t.Fatalf("parts carry %d spans, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i].span, want[i].span) || !proto.Equal(got[i].resource, want[i].resource) {
			t.Fatalf("span %d changed or moved off its resource", i)
		}
	}
}

// A resource too big to share maxMergeBytes with spans still reaches a host that refuses bodies
// over maxRequestBytes: the split stays under it, so every span arrives and none is refused.
func TestRelayDeliversAnExportWithAHugeResource(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	spans, refused := map[string]int{}, 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if len(body) > maxRequestBytes {
			refused++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var td tracepb.TracesData
		if err := proto.Unmarshal(body, &td); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, ps := range placedSpans(&td) {
			spans[string(ps.span.GetSpanId())]++
		}
	}))
	t.Cleanup(host.Close)
	f := newFixture()
	r, srv := f.relay(t, nil, map[string]Policy{"p1": {Endpoint: host.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}})
	// One resource of about 2.25 MiB and one scope with 3,000 spans: 5.3 MB in all.
	var all []*tracepb.Span
	for _, ps := range placedSpans(bigTraces(3000, 1000)) {
		all = append(all, ps.span)
	}
	td := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "svc"), kv("huge", strings.Repeat("r", 2300<<10))}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "scope"}, Spans: all}},
	}}}
	postProto(t, srv, "/v1/traces", td)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] == 3000 })
	mu.Lock()
	defer mu.Unlock()
	if refused != 0 || len(spans) != 3000 {
		t.Fatalf("host refused %d requests and received %d distinct spans, want 0 and 3000", refused, len(spans))
	}
	for id, n := range spans {
		if n != 1 {
			t.Fatalf("span %s arrived %d times", id, n)
		}
	}
	if c := r.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("dropped records: %v", c)
	}
}
