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

// run is records [from, to) of block b: one resource and scope's records.
type run struct{ block, from, to int }

// within divides a part whose encoding is over prefer into parts for the host, record by record
// in order, each record under a copy of its own resource and scope, as packRuns lays them out.
// rejected counts the records no request the host accepts can carry; the caller drops them.
func (p *part) within(prefer int) (parts []*part, rejected int) {
	if proto.Size(p.msg) <= prefer {
		return []*part{p}, 0
	}
	with := func(msg proto.Message, records int) {
		q := *p
		q.msg, q.records = msg, records
		parts = append(parts, &q)
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
		msgs, out := packRuns(prefer, maxRequestBytes, heads, sizes)
		for _, runs := range msgs {
			td := &tracepb.TracesData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				td.ResourceSpans = append(td.ResourceSpans, &tracepb.ResourceSpans{
					Resource:   cloneResource(b.rs.GetResource()),
					SchemaUrl:  b.rs.GetSchemaUrl(),
					ScopeSpans: []*tracepb.ScopeSpans{{Scope: b.ss.GetScope(), SchemaUrl: b.ss.GetSchemaUrl(), Spans: b.ss.GetSpans()[r.from:r.to]}},
				})
				n += r.to - r.from
			}
			with(td, n)
		}
		for _, r := range out {
			rejected += r.to - r.from
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
		msgs, out := packRuns(prefer, maxRequestBytes, heads, sizes)
		for _, runs := range msgs {
			ld := &logspb.LogsData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				ld.ResourceLogs = append(ld.ResourceLogs, &logspb.ResourceLogs{
					Resource:  cloneResource(b.rl.GetResource()),
					SchemaUrl: b.rl.GetSchemaUrl(),
					ScopeLogs: []*logspb.ScopeLogs{{Scope: b.sl.GetScope(), SchemaUrl: b.sl.GetSchemaUrl(), LogRecords: b.sl.GetLogRecords()[r.from:r.to]}},
				})
				n += r.to - r.from
			}
			with(ld, n)
		}
		for _, r := range out {
			rejected += r.to - r.from
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
		points := func(metrics []*metricspb.Metric) (n int) {
			for _, mt := range metrics {
				eachPointTime(mt, func(uint64) { n++ })
			}
			return n
		}
		msgs, out := packRuns(prefer, maxRequestBytes, heads, sizes)
		for _, runs := range msgs {
			md := &metricspb.MetricsData{}
			n := 0
			for _, r := range runs {
				b := blocks[r.block]
				metrics := b.sm.GetMetrics()[r.from:r.to]
				md.ResourceMetrics = append(md.ResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     cloneResource(b.rm.GetResource()),
					SchemaUrl:    b.rm.GetSchemaUrl(),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.sm.GetScope(), SchemaUrl: b.sm.GetSchemaUrl(), Metrics: metrics}},
				})
				n += points(metrics)
			}
			with(md, n)
		}
		for _, r := range out {
			rejected += points(blocks[r.block].sm.GetMetrics()[r.from:r.to])
		}
	default:
		return []*part{p}, 0
	}
	return parts, rejected
}

// packRuns lays every block's records out, in order, into messages for a host that refuses one
// over hard, preferring messages within prefer. heads[b] is block b's framed resource and scope
// and sizes[b] its framed records; a message repeats the header of every block it carries.
//
//   - A block whose header is at most half of prefer packs to prefer, sharing messages with its
//     neighbours. Two consecutive messages of the block then carry more of its record bytes than
//     one header, so its share of the messages is at most three times its own size.
//   - A block with a larger header packs alone to hard, and is rejected whole when that would
//     still copy its header past three times its size.
//   - A record too large to share a message goes alone; one that cannot fit hard beside its
//     header is rejected.
func packRuns(prefer, hard int, heads []int, sizes [][]int) (msgs [][]run, rejected []run) {
	size, sealed := 0, true // sealed: the next record opens a message
	for b, recs := range sizes {
		h := heads[b]
		if 2*h > prefer {
			own, out := packAlone(b, hard, h, recs)
			msgs, rejected = append(msgs, own...), append(rejected, out...)
			sealed = true
			continue
		}
		open := false
		for i, n := range recs {
			if h+n > hard {
				rejected = append(rejected, run{block: b, from: i, to: i + 1})
				open = false
				continue
			}
			need := n
			if !open {
				need += h
			}
			if sealed || size+need > prefer {
				msgs = append(msgs, nil)
				size, sealed, open, need = 0, false, false, n+h
			}
			cur := &msgs[len(msgs)-1]
			if !open {
				*cur = append(*cur, run{block: b, from: i})
				open = true
			}
			(*cur)[len(*cur)-1].to = i + 1
			size += need
		}
	}
	return msgs, rejected
}

// packAlone packs a header-heavy block b in messages of its own up to hard, rejecting each
// record that cannot fit beside the header, or every record when the messages would total over
// three times the block.
func packAlone(b, hard, h int, recs []int) (msgs [][]run, rejected []run) {
	kept, total, size := 0, 0, 0
	for i, n := range recs {
		if h+n > hard {
			rejected = append(rejected, run{block: b, from: i, to: i + 1})
			size = 0
			continue
		}
		if size == 0 || size+n > hard {
			msgs = append(msgs, []run{{block: b, from: i}})
			size, total = h, total+h
		}
		msgs[len(msgs)-1][0].to = i + 1
		size, total, kept = size+n, total+n, kept+n
	}
	if total > 3*(h+kept) {
		return nil, []run{{block: b, from: 0, to: len(recs)}}
	}
	return msgs, rejected
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
