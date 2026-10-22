package relay

import (
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// recordFraming bounds the tag and length prefix protobuf puts before one nested message.
	recordFraming = 6
	// maxRequestBytes is the largest body the OTLP host accepts; it refuses a larger one.
	maxRequestBytes = 4 << 20
)

// within divides a part whose encoding is over limit into parts that each fit it, record by
// record in order, each record under its own resource and scope. A record larger than limit
// goes alone, a host refusing it counted like any refused part, and a resource or scope over
// half the limit packs as packRuns says.
func (p *part) within(limit int) []*part {
	if proto.Size(p.msg) <= limit {
		return []*part{p}
	}
	var out []*part
	with := func(msg proto.Message, records int) {
		q := *p
		q.msg, q.records = msg, records
		out = append(out, &q)
	}
	switch m := p.msg.(type) {
	case *tracepb.TracesData:
		type block struct {
			rs *tracepb.ResourceSpans
			ss *tracepb.ScopeSpans
		}
		var blocks []block
		var heads []int
		var sizes [][]int
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				blocks = append(blocks, block{rs, ss})
				heads = append(heads, framedHead(rs.GetResource(), rs.GetSchemaUrl(), ss.GetScope(), ss.GetSchemaUrl()))
				sizes = append(sizes, framedSizes(ss.GetSpans()))
			}
		}
		for _, runs := range packRuns(limit, heads, sizes) {
			out := &tracepb.TracesData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				out.ResourceSpans = append(out.ResourceSpans, &tracepb.ResourceSpans{
					Resource:   cloneResource(b.rs.GetResource()),
					SchemaUrl:  b.rs.GetSchemaUrl(),
					ScopeSpans: []*tracepb.ScopeSpans{{Scope: b.ss.GetScope(), SchemaUrl: b.ss.GetSchemaUrl(), Spans: b.ss.GetSpans()[r.from:r.to]}},
				})
				n += r.to - r.from
			}
			with(out, n)
		}
	case *logspb.LogsData:
		type block struct {
			rl *logspb.ResourceLogs
			sl *logspb.ScopeLogs
		}
		var blocks []block
		var heads []int
		var sizes [][]int
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				blocks = append(blocks, block{rl, sl})
				heads = append(heads, framedHead(rl.GetResource(), rl.GetSchemaUrl(), sl.GetScope(), sl.GetSchemaUrl()))
				sizes = append(sizes, framedSizes(sl.GetLogRecords()))
			}
		}
		for _, runs := range packRuns(limit, heads, sizes) {
			out := &logspb.LogsData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				out.ResourceLogs = append(out.ResourceLogs, &logspb.ResourceLogs{
					Resource:  cloneResource(b.rl.GetResource()),
					SchemaUrl: b.rl.GetSchemaUrl(),
					ScopeLogs: []*logspb.ScopeLogs{{Scope: b.sl.GetScope(), SchemaUrl: b.sl.GetSchemaUrl(), LogRecords: b.sl.GetLogRecords()[r.from:r.to]}},
				})
				n += r.to - r.from
			}
			with(out, n)
		}
	case *metricspb.MetricsData:
		type block struct {
			rm *metricspb.ResourceMetrics
			sm *metricspb.ScopeMetrics
		}
		var blocks []block
		var heads []int
		var sizes [][]int
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				blocks = append(blocks, block{rm, sm})
				heads = append(heads, framedHead(rm.GetResource(), rm.GetSchemaUrl(), sm.GetScope(), sm.GetSchemaUrl()))
				sizes = append(sizes, framedSizes(sm.GetMetrics()))
			}
		}
		for _, runs := range packRuns(limit, heads, sizes) {
			out := &metricspb.MetricsData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				metrics := b.sm.GetMetrics()[r.from:r.to]
				out.ResourceMetrics = append(out.ResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     cloneResource(b.rm.GetResource()),
					SchemaUrl:    b.rm.GetSchemaUrl(),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.sm.GetScope(), SchemaUrl: b.sm.GetSchemaUrl(), Metrics: metrics}},
				})
				for _, mt := range metrics {
					eachPointTime(mt, func(uint64) { n++ })
				}
			}
			with(out, n)
		}
	default:
		return []*part{p}
	}
	return out
}

// run is records [from, to) of block b: one resource and scope's records.
type run struct{ block, from, to int }

// packRuns lays every block's records out in order into messages whose encoding stays within
// limit, repeating a block's resource and scope in each message it spans. heads[b] is block b's
// framed resource and scope, sizes[b] its framed records. A record over limit goes alone. A
// block whose header is over half the limit packs to twice its header instead, so the header is
// not copied once per record, though never past a merge short of maxRequestBytes while the
// header fits there.
func packRuns(limit int, heads []int, sizes [][]int) [][]run {
	var msgs [][]run
	size, records := 0, 0
	// The ceiling leaves a merge's room under the host's limit for send-time withholding.
	ceiling := maxRequestBytes - limit
	for b, recs := range sizes {
		fit := max(limit, 2*heads[b])
		if heads[b] < ceiling {
			fit = min(fit, ceiling)
		}
		open := false
		for i, n := range recs {
			need := n
			if !open {
				need += heads[b]
			}
			if msgs == nil || records > 0 && size+need > fit {
				msgs = append(msgs, nil)
				size, records, open, need = 0, 0, false, n+heads[b]
			}
			cur := &msgs[len(msgs)-1]
			if !open {
				*cur = append(*cur, run{block: b, from: i})
				open = true
			}
			(*cur)[len(*cur)-1].to = i + 1
			size += need
			records++
		}
	}
	return msgs
}

func framedHead(resource proto.Message, resourceURL string, scope proto.Message, scopeURL string) int {
	return proto.Size(resource) + len(resourceURL) + proto.Size(scope) + len(scopeURL) + 6*recordFraming
}

func framedSizes[M proto.Message](records []M) []int {
	out := make([]int, len(records))
	for i, r := range records {
		out[i] = proto.Size(r) + recordFraming
	}
	return out
}
