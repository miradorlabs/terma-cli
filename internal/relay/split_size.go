package relay

import (
	"fmt"

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

// layout is an export cut into blocks, one per resource and scope, for packRuns to lay out:
// heads[b] is block b's framed resource and scope and sizes[b] its framed records. build
// reassembles runs of records, each run under a copy of its block's resource and its scope and
// schema URLs; count is their records (spans, log records or metric data points). heads and
// sizes bound the encoding from above; alone is the exact size of one record's message.
type layout struct {
	heads []int
	sizes [][]int
	build func(runs []run) proto.Message
	count func(runs []run) int
}

func (l layout) alone(b, i int) int {
	return proto.Size(l.build([]run{{block: b, from: i, to: i + 1}}))
}

// headSplit is a queued body cut for sending: the filtered request to send now, what of the
// stored body stays queued after it, and the records no request the host accepts can carry.
type headSplit struct {
	body        []byte // the filtered piece to send; nil when no piece of the part fits the host
	stored      []byte // the same piece as queued, for the policy to be applied to it alone
	sent        int
	rest        []byte
	restRecords int
	tooLarge    int
}

// within divides a part whose encoding is over prefer into parts for the host, in order, as
// packRuns lays them out. rejected counts the records no request the host accepts can carry;
// the caller drops them.
func (p *part) within(prefer int) (parts []*part, rejected int) {
	if proto.Size(p.msg) <= prefer {
		return []*part{p}, 0
	}
	l, ok := layoutOf(p.msg)
	if !ok {
		return []*part{p}, 0
	}
	msgs, out := packRuns(prefer, maxRequestBytes, l.heads, l.sizes, l.alone)
	for _, runs := range msgs {
		q := *p
		q.msg, q.records = l.build(runs), l.count(runs)
		parts = append(parts, &q)
	}
	return parts, l.count(out)
}

// splitHead cuts a queued body whose filtered form is over prefer: the first message packRuns
// lays out of the filtered body is the request, and the stored body's records after it stay
// queued. Withholding changes records in place and never drops or reorders them, so the stored
// and filtered bodies share their blocks; when they do not, the rest is kept filtered, which
// sends nothing a stricter policy would withhold.
//
// Accepted risk: each piece decodes and re-encodes what is left of the part, so a part of n
// bytes costs about n²/(2·prefer) bytes of work, some 40 MiB for a 6.5 MiB one. Only parts an
// older relay or a stricter policy left over the bound take this path. Upgrade path: keep the
// decoded part across its pieces.
func splitHead(sig Signal, stored, filtered []byte, prefer int) (headSplit, error) {
	sm, fm := newSignalMessage(sig), newSignalMessage(sig)
	if err := proto.Unmarshal(stored, sm); err != nil {
		return headSplit{}, fmt.Errorf("decode queued %s: %w", sig, err)
	}
	if err := proto.Unmarshal(filtered, fm); err != nil {
		return headSplit{}, fmt.Errorf("decode filtered %s: %w", sig, err)
	}
	lf, _ := layoutOf(fm)
	ls, _ := layoutOf(sm)
	if len(ls.sizes) != len(lf.sizes) {
		ls = lf
	}
	for b := range ls.sizes {
		if len(ls.sizes[b]) != len(lf.sizes[b]) {
			ls = lf
			break
		}
	}
	msgs, out := packRuns(prefer, maxRequestBytes, lf.heads, lf.sizes, lf.alone)
	h := headSplit{tooLarge: lf.count(out)}
	if len(msgs) == 0 {
		return h, nil
	}
	body, err := proto.Marshal(lf.build(msgs[0]))
	if err != nil {
		return headSplit{}, err
	}
	h.body, h.sent = body, lf.count(msgs[0])
	if h.stored, err = proto.Marshal(ls.build(msgs[0])); err != nil {
		return headSplit{}, err
	}
	if rest := coalesce(msgs[1:]); len(rest) > 0 {
		if h.rest, err = proto.Marshal(ls.build(rest)); err != nil {
			return headSplit{}, err
		}
		h.restRecords = ls.count(rest)
	}
	return h, nil
}

// coalesce joins messages' runs into one list, merging each run with the one before it when
// they are consecutive records of the same block, so a block's records keep a single header.
func coalesce(msgs [][]run) []run {
	var out []run
	for _, runs := range msgs {
		for _, r := range runs {
			if n := len(out); n > 0 && out[n-1].block == r.block && out[n-1].to == r.from {
				out[n-1].to = r.to
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

func newSignalMessage(sig Signal) proto.Message {
	switch sig {
	case Logs:
		return &logspb.LogsData{}
	case Traces:
		return &tracepb.TracesData{}
	default:
		return &metricspb.MetricsData{}
	}
}

// layoutOf cuts a traces, logs or metrics message into its blocks.
func layoutOf(msg proto.Message) (layout, bool) {
	var l layout
	switch m := msg.(type) {
	case *tracepb.TracesData:
		type block struct {
			rs *tracepb.ResourceSpans
			ss *tracepb.ScopeSpans
		}
		var blocks []block
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				blocks = append(blocks, block{rs, ss})
				l.heads = append(l.heads, framedHead(rs.GetResource(), rs.GetSchemaUrl(), ss.GetScope(), ss.GetSchemaUrl()))
				l.sizes = append(l.sizes, framedSizes(ss.GetSpans()))
			}
		}
		l.build = func(runs []run) proto.Message {
			td := &tracepb.TracesData{}
			for _, r := range runs {
				b := blocks[r.block]
				td.ResourceSpans = append(td.ResourceSpans, &tracepb.ResourceSpans{
					Resource:   cloneResource(b.rs.GetResource()),
					SchemaUrl:  b.rs.GetSchemaUrl(),
					ScopeSpans: []*tracepb.ScopeSpans{{Scope: b.ss.GetScope(), SchemaUrl: b.ss.GetSchemaUrl(), Spans: b.ss.GetSpans()[r.from:r.to]}},
				})
			}
			return td
		}
		l.count = countRecords
	case *logspb.LogsData:
		type block struct {
			rl *logspb.ResourceLogs
			sl *logspb.ScopeLogs
		}
		var blocks []block
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				blocks = append(blocks, block{rl, sl})
				l.heads = append(l.heads, framedHead(rl.GetResource(), rl.GetSchemaUrl(), sl.GetScope(), sl.GetSchemaUrl()))
				l.sizes = append(l.sizes, framedSizes(sl.GetLogRecords()))
			}
		}
		l.build = func(runs []run) proto.Message {
			ld := &logspb.LogsData{}
			for _, r := range runs {
				b := blocks[r.block]
				ld.ResourceLogs = append(ld.ResourceLogs, &logspb.ResourceLogs{
					Resource:  cloneResource(b.rl.GetResource()),
					SchemaUrl: b.rl.GetSchemaUrl(),
					ScopeLogs: []*logspb.ScopeLogs{{Scope: b.sl.GetScope(), SchemaUrl: b.sl.GetSchemaUrl(), LogRecords: b.sl.GetLogRecords()[r.from:r.to]}},
				})
			}
			return ld
		}
		l.count = countRecords
	case *metricspb.MetricsData:
		type block struct {
			rm *metricspb.ResourceMetrics
			sm *metricspb.ScopeMetrics
		}
		var blocks []block
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				blocks = append(blocks, block{rm, sm})
				l.heads = append(l.heads, framedHead(rm.GetResource(), rm.GetSchemaUrl(), sm.GetScope(), sm.GetSchemaUrl()))
				l.sizes = append(l.sizes, framedSizes(sm.GetMetrics()))
			}
		}
		l.build = func(runs []run) proto.Message {
			md := &metricspb.MetricsData{}
			for _, r := range runs {
				b := blocks[r.block]
				md.ResourceMetrics = append(md.ResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     cloneResource(b.rm.GetResource()),
					SchemaUrl:    b.rm.GetSchemaUrl(),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.sm.GetScope(), SchemaUrl: b.sm.GetSchemaUrl(), Metrics: b.sm.GetMetrics()[r.from:r.to]}},
				})
			}
			return md
		}
		l.count = func(runs []run) (n int) {
			for _, r := range runs {
				for _, mt := range blocks[r.block].sm.GetMetrics()[r.from:r.to] {
					eachPointTime(mt, func(uint64) { n++ })
				}
			}
			return n
		}
	default:
		return layout{}, false
	}
	return l, true
}

func countRecords(runs []run) (n int) {
	for _, r := range runs {
		n += r.to - r.from
	}
	return n
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
//   - A record too large to share a message goes alone; one whose message alone, exactly as
//     encoded (alone), is over hard is rejected.
func packRuns(prefer, hard int, heads []int, sizes [][]int, alone func(b, i int) int) (msgs [][]run, rejected []run) {
	size, sealed := 0, true // sealed: the next record opens a message
	for b, recs := range sizes {
		h := heads[b]
		if 2*h > prefer {
			own, out := packAlone(b, hard, h, recs, alone)
			msgs, rejected = append(msgs, own...), append(rejected, out...)
			sealed = true
			continue
		}
		open := false
		for i, n := range recs {
			if h+n > hard && alone(b, i) > hard {
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
func packAlone(b, hard, h int, recs []int, alone func(b, i int) int) (msgs [][]run, rejected []run) {
	kept, total, size := 0, 0, 0
	for i, n := range recs {
		if h+n > hard && alone(b, i) > hard {
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
