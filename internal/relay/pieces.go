package relay

import (
	"math"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// pieces splits p, on a migrated machine, by the placement each of its records was made
// under, so that a part spanning placements is routed piece by piece, each stamped and
// filtered for its own team ("Where a record ran"). Under the single-team rules, or where
// the session's placements do not divide p, p is its one piece, routed by its earliest
// record as before.
func (r *Relay) pieces(key string, p *part) []*part {
	if !r.teams() {
		return []*part{p}
	}
	session := r.sessionFor(key)
	if session == "" || strings.HasPrefix(session, procPrefix) {
		return []*part{p}
	}
	c, ok := r.lookup(session)
	if !ok || len(c.Placements) < 2 {
		return []*part{p}
	}
	keyOf := func(t uint64) string {
		var at time.Time
		if t != 0 && t <= math.MaxInt64 {
			at = time.Unix(0, int64(t))
		}
		pl, ok := c.At(p.pid, at)
		if !ok {
			return "\x00unplaced"
		}
		return strings.Join([]string{string(pl.Kind), pl.ProjectID, pl.Run, pl.Repository.Origin, pl.Root, pl.Cwd, pl.Tool}, "\x00")
	}
	groups, order := splitBy(p, keyOf)
	if len(order) < 2 {
		return []*part{p}
	}
	out := make([]*part, 0, len(order))
	for _, k := range order {
		piece := *p
		piece.msg, piece.records = groups[k].msg, groups[k].records
		piece.at = earliest(piece.msg)
		out = append(out, &piece)
	}
	return out
}

// piece is one placement's records of a part.
type piece struct {
	msg     proto.Message
	records int
}

func newPiece(sig Signal) *piece {
	switch sig {
	case Logs:
		return &piece{msg: &logspb.LogsData{}}
	case Traces:
		return &piece{msg: &tracepb.TracesData{}}
	default:
		return &piece{msg: &metricspb.MetricsData{}}
	}
}

// groupBy partitions items by keyOf of each one's time, and lists the keys in the order
// their first item came.
func groupBy[T any](items []T, timeOf func(T) uint64, keyOf func(uint64) string) (map[string][]T, []string) {
	by := map[string][]T{}
	var keys []string
	for _, it := range items {
		k := keyOf(timeOf(it))
		if _, seen := by[k]; !seen {
			keys = append(keys, k)
		}
		by[k] = append(by[k], it)
	}
	return by, keys
}

// splitMetricBy partitions a metric's data points by keyOf of each point's time, as
// splitMetric does by session.
func splitMetricBy(m *metricspb.Metric, keyOf func(uint64) string) (map[string]metricPiece, []string) {
	out := map[string]metricPiece{}
	shell := func() *metricspb.Metric {
		return &metricspb.Metric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit(), Metadata: m.GetMetadata()}
	}
	var keys []string
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		by, ks := groupBy(d.Sum.GetDataPoints(), (*metricspb.NumberDataPoint).GetTimeUnixNano, keyOf)
		for _, k := range ks {
			c := shell()
			c.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: by[k], AggregationTemporality: d.Sum.GetAggregationTemporality(), IsMonotonic: d.Sum.GetIsMonotonic()}}
			out[k] = metricPiece{metric: c, points: len(by[k])}
		}
		keys = ks
	case *metricspb.Metric_Gauge:
		by, ks := groupBy(d.Gauge.GetDataPoints(), (*metricspb.NumberDataPoint).GetTimeUnixNano, keyOf)
		for _, k := range ks {
			c := shell()
			c.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: by[k]}}
			out[k] = metricPiece{metric: c, points: len(by[k])}
		}
		keys = ks
	case *metricspb.Metric_Histogram:
		by, ks := groupBy(d.Histogram.GetDataPoints(), (*metricspb.HistogramDataPoint).GetTimeUnixNano, keyOf)
		for _, k := range ks {
			c := shell()
			c.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: by[k], AggregationTemporality: d.Histogram.GetAggregationTemporality()}}
			out[k] = metricPiece{metric: c, points: len(by[k])}
		}
		keys = ks
	case *metricspb.Metric_ExponentialHistogram:
		by, ks := groupBy(d.ExponentialHistogram.GetDataPoints(), (*metricspb.ExponentialHistogramDataPoint).GetTimeUnixNano, keyOf)
		for _, k := range ks {
			c := shell()
			c.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: by[k], AggregationTemporality: d.ExponentialHistogram.GetAggregationTemporality()}}
			out[k] = metricPiece{metric: c, points: len(by[k])}
		}
		keys = ks
	case *metricspb.Metric_Summary:
		by, ks := groupBy(d.Summary.GetDataPoints(), (*metricspb.SummaryDataPoint).GetTimeUnixNano, keyOf)
		for _, k := range ks {
			c := shell()
			c.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: by[k]}}
			out[k] = metricPiece{metric: c, points: len(by[k])}
		}
		keys = ks
	}
	return out, keys
}

// mixed drops, on learning that a part's run is Mixed, that run's queued entries made
// from the Mixed time on: the recheck before sending would drop them too, from the run
// ledger, but a relay that knows says so at once.
func (r *Relay) mixed(v verdict) {
	if !v.mixedAt.IsZero() && v.session != "" {
		r.dropMixed(v.session, v.mixedAt)
	}
}

// splitBy partitions p's records by keyOf of each record's time, keeping each piece's
// resources and scopes, each piece with resources of its own so one team's stamps never
// reach another's. order is the keys in the order their first record came.
func splitBy(p *part, keyOf func(uint64) string) (map[string]*piece, []string) {
	out := map[string]*piece{}
	var order []string
	get := func(k string) *piece {
		pc := out[k]
		if pc == nil {
			pc = newPiece(p.signal)
			out[k] = pc
			order = append(order, k)
		}
		return pc
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				by, keys := groupBy(sl.GetLogRecords(), func(lr *logspb.LogRecord) uint64 {
					if t := lr.GetTimeUnixNano(); t != 0 {
						return t
					}
					return lr.GetObservedTimeUnixNano()
				}, keyOf)
				for _, k := range keys {
					pc := get(k)
					l := pc.msg.(*logspb.LogsData)
					l.ResourceLogs = append(l.ResourceLogs, &logspb.ResourceLogs{Resource: cloneResource(rl.GetResource()), SchemaUrl: rl.GetSchemaUrl(),
						ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: by[k]}}})
					pc.records += len(by[k])
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				by, keys := groupBy(ss.GetSpans(), (*tracepb.Span).GetStartTimeUnixNano, keyOf)
				for _, k := range keys {
					pc := get(k)
					t := pc.msg.(*tracepb.TracesData)
					t.ResourceSpans = append(t.ResourceSpans, &tracepb.ResourceSpans{Resource: cloneResource(rs.GetResource()), SchemaUrl: rs.GetSchemaUrl(),
						ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.GetScope(), SchemaUrl: ss.GetSchemaUrl(), Spans: by[k]}}})
					pc.records += len(by[k])
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				byKey := map[string][]*metricspb.Metric{}
				counts := map[string]int{}
				var keys []string
				for _, mt := range sm.GetMetrics() {
					pieces, pk := splitMetricBy(mt, keyOf)
					for _, k := range pk {
						if _, seen := byKey[k]; !seen {
							keys = append(keys, k)
						}
						byKey[k] = append(byKey[k], pieces[k].metric)
						counts[k] += pieces[k].points
					}
				}
				for _, k := range keys {
					pc := get(k)
					md := pc.msg.(*metricspb.MetricsData)
					md.ResourceMetrics = append(md.ResourceMetrics, &metricspb.ResourceMetrics{Resource: cloneResource(rm.GetResource()), SchemaUrl: rm.GetSchemaUrl(),
						ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: sm.GetScope(), SchemaUrl: sm.GetSchemaUrl(), Metrics: byKey[k]}}})
					pc.records += counts[k]
				}
			}
		}
	}
	return out, order
}
