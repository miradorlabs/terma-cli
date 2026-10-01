package relay

import (
	"encoding/hex"
	"math"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Signal is one of the three OTLP export paths.
type Signal string

// The OTLP signals, named as their export paths: /v1/<signal>.
const (
	Logs    Signal = "logs"
	Metrics Signal = "metrics"
	Traces  Signal = "traces"
)

// Exports decode as LogsData, MetricsData and TracesData, wire-identical to the requests,
// because the collector packages would link gRPC into every hook.

// part is the slice of one export request that belongs to one session.
type part struct {
	signal  Signal
	session string
	msg     proto.Message
	records int
	// pid is the process that exported it, 0 when unknown.
	pid int
	// at picks the placement of a resumed session when its process does not decide (claim.At).
	at time.Time
	// start marks a conversation start, which waits as long as a trace: its claiming hook may be far off.
	start bool
}

// sessionOf is the first session key, by rank, on the record's attributes, then its resource's.
func (ru *rules) sessionOf(attrs, resource []*commonpb.KeyValue) string {
	for _, set := range [][]*commonpb.KeyValue{attrs, resource} {
		for _, key := range ru.sessionKeys {
			for _, kv := range set {
				if kv.GetKey() == key.Attr {
					if v := kv.GetValue().GetStringValue(); v != "" && (!key.RejectNumeric || !numeric(v)) {
						return v
					}
				}
			}
		}
	}
	return ""
}

func numeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// splitLogs divides a logs export by session, records naming none under the empty key.
// A record naming its session and a trace teaches learn the trace's session, or an agent
// that names the session only on its turn-end span would lose a long turn's child spans.
func (ru *rules) splitLogs(req *logspb.LogsData, learn func(traceID, session string)) map[string]*part {
	out := map[string]*part{}
	for _, rl := range req.GetResourceLogs() {
		res := rl.GetResource().GetAttributes()
		for _, sl := range rl.GetScopeLogs() {
			bySession := map[string][]*logspb.LogRecord{}
			var order []string
			for _, lr := range sl.GetLogRecords() {
				s := ru.sessionOf(lr.GetAttributes(), res)
				if s != "" && len(lr.GetTraceId()) > 0 {
					learn(hex.EncodeToString(lr.GetTraceId()), s)
				}
				if _, seen := bySession[s]; !seen {
					order = append(order, s)
				}
				bySession[s] = append(bySession[s], lr)
			}
			for _, s := range order {
				p := out[s]
				if p == nil {
					p = &part{signal: Logs, session: s, msg: &logspb.LogsData{}}
					out[s] = p
				}
				m := p.msg.(*logspb.LogsData)
				m.ResourceLogs = append(m.ResourceLogs, &logspb.ResourceLogs{
					Resource:  cloneResource(rl.GetResource()),
					SchemaUrl: rl.GetSchemaUrl(),
					ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: bySession[s]}},
				})
				p.records += len(bySession[s])
			}
		}
	}
	return out
}

// tracePrefix keys spans of a trace whose session is not yet known.
const tracePrefix = "trace:"

// splitTraces divides a traces export by session, span by span; a span naming no session
// belongs to its trace's, since children inherit the trace, not the attribute.
func (ru *rules) splitTraces(req *tracepb.TracesData, learn func(traceID, session string), known func(traceID string) string) map[string]*part {
	for _, rs := range req.GetResourceSpans() {
		res := rs.GetResource().GetAttributes()
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if s := ru.sessionOf(sp.GetAttributes(), res); s != "" && len(sp.GetTraceId()) > 0 {
					learn(hex.EncodeToString(sp.GetTraceId()), s)
				}
			}
		}
	}
	out := map[string]*part{}
	for _, rs := range req.GetResourceSpans() {
		res := rs.GetResource().GetAttributes()
		for _, ss := range rs.GetScopeSpans() {
			bySession := map[string][]*tracepb.Span{}
			var order []string
			for _, sp := range ss.GetSpans() {
				s := ru.sessionOf(sp.GetAttributes(), res)
				if s == "" && len(sp.GetTraceId()) > 0 {
					id := hex.EncodeToString(sp.GetTraceId())
					if s = known(id); s == "" {
						s = tracePrefix + id
					}
				}
				if _, seen := bySession[s]; !seen {
					order = append(order, s)
				}
				bySession[s] = append(bySession[s], sp)
			}
			for _, s := range order {
				p := out[s]
				if p == nil {
					p = &part{signal: Traces, session: s, msg: &tracepb.TracesData{}}
					out[s] = p
				}
				m := p.msg.(*tracepb.TracesData)
				m.ResourceSpans = append(m.ResourceSpans, &tracepb.ResourceSpans{
					Resource:   cloneResource(rs.GetResource()),
					SchemaUrl:  rs.GetSchemaUrl(),
					ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.GetScope(), SchemaUrl: ss.GetSchemaUrl(), Spans: bySession[s]}},
				})
				p.records += len(bySession[s])
			}
		}
	}
	return out
}

// splitMetrics divides a metrics export by data point, since one metric's points can name several sessions.
func (ru *rules) splitMetrics(req *metricspb.MetricsData) map[string]*part {
	out := map[string]*part{}
	for _, rm := range req.GetResourceMetrics() {
		res := rm.GetResource().GetAttributes()
		for _, sm := range rm.GetScopeMetrics() {
			bySession := map[string][]*metricspb.Metric{}
			counts := map[string]int{}
			var order []string
			for _, m := range sm.GetMetrics() {
				for s, piece := range ru.splitMetric(m, res) {
					if _, seen := bySession[s]; !seen {
						order = append(order, s)
					}
					bySession[s] = append(bySession[s], piece.metric)
					counts[s] += piece.points
				}
			}
			for _, s := range order {
				p := out[s]
				if p == nil {
					p = &part{signal: Metrics, session: s, msg: &metricspb.MetricsData{}}
					out[s] = p
				}
				m := p.msg.(*metricspb.MetricsData)
				m.ResourceMetrics = append(m.ResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     cloneResource(rm.GetResource()),
					SchemaUrl:    rm.GetSchemaUrl(),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: sm.GetScope(), SchemaUrl: sm.GetSchemaUrl(), Metrics: bySession[s]}},
				})
				p.records += counts[s]
			}
		}
	}
	return out
}

type metricPiece struct {
	metric *metricspb.Metric
	points int
}

func (ru *rules) splitMetric(m *metricspb.Metric, res []*commonpb.KeyValue) map[string]metricPiece {
	out := map[string]metricPiece{}
	shell := func() *metricspb.Metric {
		return &metricspb.Metric{Name: m.GetName(), Description: m.GetDescription(), Unit: m.GetUnit(), Metadata: m.GetMetadata()}
	}
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		groups := map[string][]*metricspb.NumberDataPoint{}
		for _, dp := range d.Sum.GetDataPoints() {
			s := ru.sessionOf(dp.GetAttributes(), res)
			groups[s] = append(groups[s], dp)
		}
		for s, pts := range groups {
			c := shell()
			c.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: pts, AggregationTemporality: d.Sum.GetAggregationTemporality(), IsMonotonic: d.Sum.GetIsMonotonic()}}
			out[s] = metricPiece{c, len(pts)}
		}
	case *metricspb.Metric_Gauge:
		groups := map[string][]*metricspb.NumberDataPoint{}
		for _, dp := range d.Gauge.GetDataPoints() {
			s := ru.sessionOf(dp.GetAttributes(), res)
			groups[s] = append(groups[s], dp)
		}
		for s, pts := range groups {
			c := shell()
			c.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts}}
			out[s] = metricPiece{c, len(pts)}
		}
	case *metricspb.Metric_Histogram:
		groups := map[string][]*metricspb.HistogramDataPoint{}
		for _, dp := range d.Histogram.GetDataPoints() {
			s := ru.sessionOf(dp.GetAttributes(), res)
			groups[s] = append(groups[s], dp)
		}
		for s, pts := range groups {
			c := shell()
			c.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: pts, AggregationTemporality: d.Histogram.GetAggregationTemporality()}}
			out[s] = metricPiece{c, len(pts)}
		}
	case *metricspb.Metric_ExponentialHistogram:
		groups := map[string][]*metricspb.ExponentialHistogramDataPoint{}
		for _, dp := range d.ExponentialHistogram.GetDataPoints() {
			s := ru.sessionOf(dp.GetAttributes(), res)
			groups[s] = append(groups[s], dp)
		}
		for s, pts := range groups {
			c := shell()
			c.Data = &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: pts, AggregationTemporality: d.ExponentialHistogram.GetAggregationTemporality()}}
			out[s] = metricPiece{c, len(pts)}
		}
	case *metricspb.Metric_Summary:
		groups := map[string][]*metricspb.SummaryDataPoint{}
		for _, dp := range d.Summary.GetDataPoints() {
			s := ru.sessionOf(dp.GetAttributes(), res)
			groups[s] = append(groups[s], dp)
		}
		for s, pts := range groups {
			c := shell()
			c.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: pts}}
			out[s] = metricPiece{c, len(pts)}
		}
	}
	return out
}

// cloneResource keeps one project's stamp from reaching a part bound for another.
func cloneResource(r *resourcepb.Resource) *resourcepb.Resource {
	if r == nil {
		return &resourcepb.Resource{}
	}
	return proto.Clone(r).(*resourcepb.Resource)
}

func (ru *rules) conversationStart(p *part) bool {
	m, ok := p.msg.(*logspb.LogsData)
	if !ok {
		return false
	}
	for _, rl := range m.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				for _, kv := range lr.GetAttributes() {
					if kv.GetKey() == "event.name" && contains(ru.startEvents, kv.GetValue().GetStringValue()) {
						return true
					}
				}
			}
		}
	}
	return false
}

func earliest(msg proto.Message) time.Time {
	var first uint64
	see := func(t uint64) {
		if t != 0 && (first == 0 || t < first) {
			first = t
		}
	}
	switch m := msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					if t := lr.GetTimeUnixNano(); t != 0 {
						see(t)
					} else {
						see(lr.GetObservedTimeUnixNano())
					}
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					see(sp.GetStartTimeUnixNano())
				}
			}
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, mt := range sm.GetMetrics() {
					eachPointTime(mt, see)
				}
			}
		}
	}
	if first == 0 || first > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(0, int64(first))
}

func eachPointTime(m *metricspb.Metric, see func(uint64)) {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		for _, p := range d.Sum.GetDataPoints() {
			see(p.GetTimeUnixNano())
		}
	case *metricspb.Metric_Gauge:
		for _, p := range d.Gauge.GetDataPoints() {
			see(p.GetTimeUnixNano())
		}
	case *metricspb.Metric_Histogram:
		for _, p := range d.Histogram.GetDataPoints() {
			see(p.GetTimeUnixNano())
		}
	case *metricspb.Metric_ExponentialHistogram:
		for _, p := range d.ExponentialHistogram.GetDataPoints() {
			see(p.GetTimeUnixNano())
		}
	case *metricspb.Metric_Summary:
		for _, p := range d.Summary.GetDataPoints() {
			see(p.GetTimeUnixNano())
		}
	}
}
