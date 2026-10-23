package relay

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// layout is an export cut into blocks, one per resource and scope, for packRuns to lay out:
// heads[b] is block b's framed resource and scope and sizes[b] its framed records. build
// reassembles runs of records, each run under a copy of its block's resource and its scope and
// schema URLs; count is their records (spans, log records or metric data points). heads and
// sizes bound the encoding from above. frames[b] and record give the exact size of one record's
// request (alone) without building it, so a huge resource is never copied to size a record.
type layout struct {
	heads  []int
	sizes  [][]int
	frames []frame
	record func(b, i int) int // record i of block b, encoded
	build  func(runs []run) proto.Message
	count  func(runs []run) int
}

// frame is a block's resource and scope as a request carries them.
type frame struct {
	res, scope       int // encoded sizes; scope is -1 when the block has none
	resURL, scopeURL string
}

func (l layout) alone(b, i int) int { return l.frames[b].sole(l.record(b, i)) }

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
				l.frames = append(l.frames, frameOf(rs.GetResource(), rs.GetSchemaUrl(), ss.GetScope(), ss.GetSchemaUrl()))
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
				l.frames = append(l.frames, frameOf(rl.GetResource(), rl.GetSchemaUrl(), sl.GetScope(), sl.GetSchemaUrl()))
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
		// A metric's data points are its records, so one metric of many points is cut like a
		// scope of many spans. Each run of a metric's points goes under a copy of the metric
		// without them; the block's header counts the largest such copy once a run, as a run
		// can open mid-metric, and a metric's first point counts its own.
		type item struct{ metric, point int } // point -1: a metric with no data points
		type block struct {
			rm    *metricspb.ResourceMetrics
			sm    *metricspb.ScopeMetrics
			items []item
			subs  []func(from, to int) *metricspb.Metric
			sizes [][]int
		}
		var blocks []*block
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				b := &block{rm: rm, sm: sm}
				var sizes []int
				shells := 0
				for mi, mt := range sm.GetMetrics() {
					pts, sub := pointsOf(mt)
					b.subs, b.sizes = append(b.subs, sub), append(b.sizes, pts)
					shell := proto.Size(sub(0, 0)) + 2*recordFraming
					shells = max(shells, shell)
					if len(pts) == 0 {
						b.items, sizes = append(b.items, item{mi, -1}), append(sizes, shell)
						continue
					}
					for j, n := range pts {
						if j == 0 {
							n += shell
						}
						b.items, sizes = append(b.items, item{mi, j}), append(sizes, n)
					}
				}
				blocks = append(blocks, b)
				l.heads = append(l.heads, framedHead(rm.GetResource(), rm.GetSchemaUrl(), sm.GetScope(), sm.GetSchemaUrl())+shells)
				l.frames = append(l.frames, frameOf(rm.GetResource(), rm.GetSchemaUrl(), sm.GetScope(), sm.GetSchemaUrl()))
				l.sizes = append(l.sizes, sizes)
			}
		}
		l.build = func(runs []run) proto.Message {
			md := &metricspb.MetricsData{}
			for _, r := range runs {
				b := blocks[r.block]
				var metrics []*metricspb.Metric
				for i := r.from; i < r.to; {
					it, j := b.items[i], i+1
					for j < r.to && b.items[j].metric == it.metric {
						j++
					}
					from, to := it.point, b.items[j-1].point+1
					if it.point < 0 || from == 0 && to == len(b.sizes[it.metric]) {
						metrics = append(metrics, b.sm.GetMetrics()[it.metric])
					} else {
						metrics = append(metrics, b.subs[it.metric](from, to))
					}
					i = j
				}
				md.ResourceMetrics = append(md.ResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     cloneResource(b.rm.GetResource()),
					SchemaUrl:    b.rm.GetSchemaUrl(),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: b.sm.GetScope(), SchemaUrl: b.sm.GetSchemaUrl(), Metrics: metrics}},
				})
			}
			return md
		}
		l.record = func(b, i int) int {
			it := blocks[b].items[i]
			if it.point < 0 || len(blocks[b].sizes[it.metric]) == 1 { // build sends the metric itself
				return proto.Size(blocks[b].sm.GetMetrics()[it.metric])
			}
			return proto.Size(blocks[b].subs[it.metric](it.point, it.point+1))
		}
		l.count = func(runs []run) (n int) {
			for _, r := range runs {
				for _, it := range blocks[r.block].items[r.from:r.to] {
					if it.point >= 0 {
						n++
					}
				}
			}
			return n
		}
	default:
		return layout{}, false
	}
	if l.record == nil {
		sizes := l.sizes
		l.record = func(b, i int) int { return sizes[b][i] - recordFraming }
	}
	return l, true
}

func countRecords(runs []run) (n int) {
	for _, r := range runs {
		n += r.to - r.from
	}
	return n
}

// pointsOf is a metric's framed data point sizes, and sub the metric holding only points
// [from, to). A metric of a kind it does not know has no points, and sub is the metric whole.
func pointsOf(m *metricspb.Metric) (sizes []int, sub func(from, to int) *metricspb.Metric) {
	shell := func() *metricspb.Metric {
		return &metricspb.Metric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit(), Metadata: m.GetMetadata()}
	}
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		pts := d.Sum.GetDataPoints()
		return framedSizes(pts), func(from, to int) *metricspb.Metric {
			c := shell()
			c.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: pts[from:to], AggregationTemporality: d.Sum.GetAggregationTemporality(), IsMonotonic: d.Sum.GetIsMonotonic()}}
			return c
		}
	case *metricspb.Metric_Gauge:
		pts := d.Gauge.GetDataPoints()
		return framedSizes(pts), func(from, to int) *metricspb.Metric {
			c := shell()
			c.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts[from:to]}}
			return c
		}
	case *metricspb.Metric_Histogram:
		pts := d.Histogram.GetDataPoints()
		return framedSizes(pts), func(from, to int) *metricspb.Metric {
			c := shell()
			c.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: pts[from:to], AggregationTemporality: d.Histogram.GetAggregationTemporality()}}
			return c
		}
	case *metricspb.Metric_ExponentialHistogram:
		pts := d.ExponentialHistogram.GetDataPoints()
		return framedSizes(pts), func(from, to int) *metricspb.Metric {
			c := shell()
			c.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: pts[from:to], AggregationTemporality: d.ExponentialHistogram.GetAggregationTemporality()}}
			return c
		}
	case *metricspb.Metric_Summary:
		pts := d.Summary.GetDataPoints()
		return framedSizes(pts), func(from, to int) *metricspb.Metric {
			c := shell()
			c.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: pts[from:to]}}
			return c
		}
	}
	return nil, func(int, int) *metricspb.Metric { return m }
}

func frameOf(resource *resourcepb.Resource, resURL string, scope *commonpb.InstrumentationScope, scopeURL string) frame {
	f := frame{res: proto.Size(resource), scope: -1, resURL: resURL, scopeURL: scopeURL}
	if scope != nil {
		f.scope = proto.Size(scope)
	}
	return f
}

// sole is the exact size of a request carrying one record of rec bytes under f, as build lays it
// out: the resource (always set, a copy), its schema URL, and one scope holding the record. Every
// signal's request nests the same field numbers.
func (f frame) sole(rec int) int {
	scope := protowire.SizeTag(2) + protowire.SizeBytes(rec)
	if f.scope >= 0 {
		scope += protowire.SizeTag(1) + protowire.SizeBytes(f.scope)
	}
	if f.scopeURL != "" {
		scope += protowire.SizeTag(3) + protowire.SizeBytes(len(f.scopeURL))
	}
	res := protowire.SizeTag(1) + protowire.SizeBytes(f.res) + protowire.SizeTag(2) + protowire.SizeBytes(scope)
	if f.resURL != "" {
		res += protowire.SizeTag(3) + protowire.SizeBytes(len(f.resURL))
	}
	return protowire.SizeTag(1) + protowire.SizeBytes(res)
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
