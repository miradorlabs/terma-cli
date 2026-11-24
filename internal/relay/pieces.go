package relay

import (
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type timedPoint interface {
	GetStartTimeUnixNano() uint64
	GetTimeUnixNano() uint64
}
type grouped[T any] struct {
	k string
	v []T
}

func (r *Relay) pieces(key string, p *part) []*part {
	if !r.teams() {
		return []*part{p}
	}
	session, _ := r.sessionOf(key, p.pid)
	c, ok := r.lookup(session)
	if session == "" || !ok || len(c.Placements) < 2 {
		return []*part{p}
	}
	placeKey := func(t uint64) string {
		if pl, ok := c.At(p.pid, unixTime(t)); ok {
			return strings.Join([]string{string(pl.Kind), pl.ProjectID, pl.Run, pl.Profile, pl.Account, pl.Repository.Origin, pl.Root, pl.Cwd, pl.Tool}, "\x00")
		}
		return "\x00unplaced"
	}
	return partitionPart(p, func(iv timeInterval) string {
		ks, ke := placeKey(iv.start), placeKey(iv.end)
		return map[bool]string{true: ks + "\x01" + ke, false: ks}[iv.end != iv.start && ke != ks]
	})
}
func splitBeforeMixed(p *part, mixedAt time.Time) (before, atOrAfter *part) {
	if p.records <= 1 || mixedAt.IsZero() {
		return nil, p
	}
	parts := partitionPart(p, func(iv timeInterval) string {
		return map[bool]string{true: "b", false: "m"}[!unixTime(iv.end).IsZero() && unixTime(iv.end).Before(mixedAt)]
	})
	if len(parts) < 2 {
		return nil, p
	}
	if parts[0].endAt.IsZero() || !parts[0].endAt.Before(mixedAt) {
		parts[0], parts[1] = parts[1], parts[0]
	}
	return parts[0], parts[1]
}
func logIV(x *logspb.LogRecord) timeInterval { return makeInterval(x.GetTimeUnixNano(), x.GetObservedTimeUnixNano()) }
func spanIV(x *tracepb.Span) timeInterval { return makeInterval(x.GetStartTimeUnixNano(), x.GetEndTimeUnixNano()) }
func partitionPart(p *part, keyOf func(timeInterval) string) []*part {
	groups, out := map[string]*part{}, []*part(nil)
	get := func(k string) *part {
		if pc := groups[k]; pc != nil {
			return pc
		}
		pc := *p
		pc.msg, pc.records = map[Signal]proto.Message{Logs: &logspb.LogsData{}, Traces: &tracepb.TracesData{}, Metrics: &metricspb.MetricsData{}}[p.signal], 0
		groups[k], out = &pc, append(out, &pc)
		return &pc
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, g := range groupBy(sl.GetLogRecords(), logIV, keyOf) {
					pc := get(g.k)
					pc.msg.(*logspb.LogsData).ResourceLogs = append(pc.msg.(*logspb.LogsData).ResourceLogs, &logspb.ResourceLogs{Resource: cloneResource(rl.GetResource()), SchemaUrl: rl.GetSchemaUrl(), ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: g.v}}})
					pc.records += len(g.v)
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, g := range groupBy(ss.GetSpans(), spanIV, keyOf) {
					pc := get(g.k)
					pc.msg.(*tracepb.TracesData).ResourceSpans = append(pc.msg.(*tracepb.TracesData).ResourceSpans, &tracepb.ResourceSpans{Resource: cloneResource(rs.GetResource()), SchemaUrl: rs.GetSchemaUrl(), ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.GetScope(), SchemaUrl: ss.GetSchemaUrl(), Spans: g.v}}})
					pc.records += len(g.v)
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				smBy := map[string]*metricspb.ScopeMetrics{}
				for _, mt := range sm.GetMetrics() {
					splitMetricBy(mt, keyOf, func(k string, c *metricspb.Metric, pts int) {
						pc := get(k)
						if smBy[k] == nil {
							smBy[k] = &metricspb.ScopeMetrics{Scope: sm.GetScope(), SchemaUrl: sm.GetSchemaUrl()}
							pc.msg.(*metricspb.MetricsData).ResourceMetrics = append(pc.msg.(*metricspb.MetricsData).ResourceMetrics, &metricspb.ResourceMetrics{Resource: cloneResource(rm.GetResource()), SchemaUrl: rm.GetSchemaUrl(), ScopeMetrics: []*metricspb.ScopeMetrics{smBy[k]}})
						}
						smBy[k].Metrics, pc.records = append(smBy[k].Metrics, c), pc.records+pts
					})
				}
			}
		}
	}
	if len(out) < 2 {
		return []*part{p}
	}
	for _, pc := range out {
		pc.at, pc.endAt = intervalOf(pc.msg)
	}
	return out
}
func groupBy[T any](items []T, ivOf func(T) timeInterval, keyOf func(timeInterval) string) []grouped[T] {
	idx, out := map[string]int{}, []grouped[T](nil)
	for _, it := range items {
		k := keyOf(ivOf(it))
		if _, ok := idx[k]; !ok {
			idx[k], out = len(out), append(out, grouped[T]{k: k})
		}
		out[idx[k]].v = append(out[idx[k]].v, it)
	}
	return out
}
func splitPts[P timedPoint](m *metricspb.Metric, pts []P, keyOf func(timeInterval) string, set func(*metricspb.Metric, []P), emit func(string, *metricspb.Metric, int)) {
	for _, g := range groupBy(pts, func(p P) timeInterval { return makeInterval(p.GetStartTimeUnixNano(), p.GetTimeUnixNano()) }, keyOf) {
		c := &metricspb.Metric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit(), Metadata: m.GetMetadata()}
		set(c, g.v)
		emit(g.k, c, len(g.v))
	}
}
func splitMetricBy(m *metricspb.Metric, keyOf func(timeInterval) string, emit func(string, *metricspb.Metric, int)) {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		splitPts(m, d.Sum.GetDataPoints(), keyOf, func(c *metricspb.Metric, v []*metricspb.NumberDataPoint) {
			c.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: v, AggregationTemporality: d.Sum.GetAggregationTemporality(), IsMonotonic: d.Sum.GetIsMonotonic()}}
		}, emit)
	case *metricspb.Metric_Gauge:
		splitPts(m, d.Gauge.GetDataPoints(), keyOf, func(c *metricspb.Metric, v []*metricspb.NumberDataPoint) {
			c.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: v}}
		}, emit)
	case *metricspb.Metric_Histogram:
		splitPts(m, d.Histogram.GetDataPoints(), keyOf, func(c *metricspb.Metric, v []*metricspb.HistogramDataPoint) {
			c.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: v, AggregationTemporality: d.Histogram.GetAggregationTemporality()}}
		}, emit)
	case *metricspb.Metric_ExponentialHistogram:
		splitPts(m, d.ExponentialHistogram.GetDataPoints(), keyOf, func(c *metricspb.Metric, v []*metricspb.ExponentialHistogramDataPoint) {
			c.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: v, AggregationTemporality: d.ExponentialHistogram.GetAggregationTemporality()}}
		}, emit)
	case *metricspb.Metric_Summary:
		splitPts(m, d.Summary.GetDataPoints(), keyOf, func(c *metricspb.Metric, v []*metricspb.SummaryDataPoint) {
			c.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: v}}
		}, emit)
	}
}
