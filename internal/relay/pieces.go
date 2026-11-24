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

func (r *Relay) pieces(key string, p *part) []*part {
	if !r.teams() {
		return []*part{p}
	}
	session := r.sessionFor(key)
	c, ok := r.lookup(session)
	if session == "" || strings.HasPrefix(session, procPrefix) || !ok || len(c.Placements) < 2 {
		return []*part{p}
	}
	placeKey := func(t uint64) string {
		if pl, ok := c.At(p.pid, unixTime(t)); ok {
			return strings.Join([]string{string(pl.Kind), pl.ProjectID, pl.Run, pl.Profile, pl.Account, pl.Repository.Origin, pl.Root, pl.Cwd, pl.Tool}, "\x00")
		}
		return "\x00unplaced"
	}
	return partitionPart(p, func(iv timeInterval) string {
		ks := placeKey(iv.start)
		if iv.end != iv.start {
			if ke := placeKey(iv.end); ke != ks {
				return ks + "\x01" + ke
			}
		}
		return ks
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
	if !parts[0].endAt.IsZero() && parts[0].endAt.Before(mixedAt) {
		return parts[0], parts[1]
	}
	return parts[1], parts[0]
}

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
				by, ks := groupBy(sl.GetLogRecords(), func(x *logspb.LogRecord) timeInterval {
					return makeInterval(x.GetTimeUnixNano(), x.GetObservedTimeUnixNano())
				}, keyOf)
				for _, k := range ks {
					pc := get(k)
					pc.msg.(*logspb.LogsData).ResourceLogs = append(pc.msg.(*logspb.LogsData).ResourceLogs, &logspb.ResourceLogs{Resource: cloneResource(rl.GetResource()), SchemaUrl: rl.GetSchemaUrl(), ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: by[k]}}})
					pc.records += len(by[k])
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				by, ks := groupBy(ss.GetSpans(), func(x *tracepb.Span) timeInterval {
					return makeInterval(x.GetStartTimeUnixNano(), x.GetEndTimeUnixNano())
				}, keyOf)
				for _, k := range ks {
					pc := get(k)
					pc.msg.(*tracepb.TracesData).ResourceSpans = append(pc.msg.(*tracepb.TracesData).ResourceSpans, &tracepb.ResourceSpans{Resource: cloneResource(rs.GetResource()), SchemaUrl: rs.GetSchemaUrl(), ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.GetScope(), SchemaUrl: ss.GetSchemaUrl(), Spans: by[k]}}})
					pc.records += len(by[k])
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				byKey, counts, ks := map[string][]*metricspb.Metric{}, map[string]int{}, []string(nil)
				for _, mt := range sm.GetMetrics() {
					splitMetricBy(mt, keyOf, func(k string, c *metricspb.Metric, pts int) {
						if _, seen := byKey[k]; !seen {
							ks = append(ks, k)
						}
						byKey[k], counts[k] = append(byKey[k], c), counts[k]+pts
					})
				}
				for _, k := range ks {
					pc := get(k)
					pc.msg.(*metricspb.MetricsData).ResourceMetrics = append(pc.msg.(*metricspb.MetricsData).ResourceMetrics, &metricspb.ResourceMetrics{Resource: cloneResource(rm.GetResource()), SchemaUrl: rm.GetSchemaUrl(), ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: sm.GetScope(), SchemaUrl: sm.GetSchemaUrl(), Metrics: byKey[k]}}})
					pc.records += counts[k]
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

func groupBy[T any](items []T, ivOf func(T) timeInterval, keyOf func(timeInterval) string) (map[string][]T, []string) {
	by, keys := map[string][]T{}, []string(nil)
	for _, it := range items {
		k := keyOf(ivOf(it))
		if _, seen := by[k]; !seen {
			keys = append(keys, k)
		}
		by[k] = append(by[k], it)
	}
	return by, keys
}

func splitPoints[P timedPoint](m *metricspb.Metric, pts []P, keyOf func(timeInterval) string, emit func(string, *metricspb.Metric, int), set func(*metricspb.Metric, []P)) {
	by, ks := groupBy(pts, func(p P) timeInterval { return makeInterval(p.GetStartTimeUnixNano(), p.GetTimeUnixNano()) }, keyOf)
	for _, k := range ks {
		c := &metricspb.Metric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit(), Metadata: m.GetMetadata()}
		set(c, by[k])
		emit(k, c, len(by[k]))
	}
}

func splitMetricBy(m *metricspb.Metric, keyOf func(timeInterval) string, emit func(string, *metricspb.Metric, int)) {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		splitPoints(m, d.Sum.GetDataPoints(), keyOf, emit, func(c *metricspb.Metric, pts []*metricspb.NumberDataPoint) {
			c.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: pts, AggregationTemporality: d.Sum.GetAggregationTemporality(), IsMonotonic: d.Sum.GetIsMonotonic()}}
		})
	case *metricspb.Metric_Gauge:
		splitPoints(m, d.Gauge.GetDataPoints(), keyOf, emit, func(c *metricspb.Metric, pts []*metricspb.NumberDataPoint) {
			c.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts}}
		})
	case *metricspb.Metric_Histogram:
		splitPoints(m, d.Histogram.GetDataPoints(), keyOf, emit, func(c *metricspb.Metric, pts []*metricspb.HistogramDataPoint) {
			c.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: pts, AggregationTemporality: d.Histogram.GetAggregationTemporality()}}
		})
	case *metricspb.Metric_ExponentialHistogram:
		splitPoints(m, d.ExponentialHistogram.GetDataPoints(), keyOf, emit, func(c *metricspb.Metric, pts []*metricspb.ExponentialHistogramDataPoint) {
			c.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: pts, AggregationTemporality: d.ExponentialHistogram.GetAggregationTemporality()}}
		})
	case *metricspb.Metric_Summary:
		splitPoints(m, d.Summary.GetDataPoints(), keyOf, emit, func(c *metricspb.Metric, pts []*metricspb.SummaryDataPoint) {
			c.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: pts}}
		})
	}
}
