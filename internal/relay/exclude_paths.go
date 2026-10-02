package relay

import (
	"encoding/base64"
	"slices"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// dropExcluded removes from p the records an attribute ties to an excluded file, before
// content filtering removes those attributes, and reports how many it removed. A record
// goes with its own attributes or with the resource or scope it sits under; the request's
// other records still leave. Reflection reaches attributes wherever OTLP keeps them.
func dropExcluded(p *part, excludes func(any) bool) int {
	if excludes == nil {
		return 0
	}
	hit := func(m proto.Message) bool {
		r := m.ProtoReflect()
		return r.IsValid() && excludedIn(r, excludes)
	}
	n := 0
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		m.ResourceLogs = slices.DeleteFunc(m.ResourceLogs, func(rl *logspb.ResourceLogs) bool {
			res := hit(rl.GetResource())
			rl.ScopeLogs = slices.DeleteFunc(rl.ScopeLogs, func(sl *logspb.ScopeLogs) bool {
				sl.LogRecords = dropRecords(sl.LogRecords, res || hit(sl.GetScope()), hit, &n)
				return len(sl.LogRecords) == 0
			})
			return len(rl.ScopeLogs) == 0
		})
	case *tracepb.TracesData:
		m.ResourceSpans = slices.DeleteFunc(m.ResourceSpans, func(rs *tracepb.ResourceSpans) bool {
			res := hit(rs.GetResource())
			rs.ScopeSpans = slices.DeleteFunc(rs.ScopeSpans, func(ss *tracepb.ScopeSpans) bool {
				ss.Spans = dropRecords(ss.Spans, res || hit(ss.GetScope()), hit, &n)
				return len(ss.Spans) == 0
			})
			return len(rs.ScopeSpans) == 0
		})
	case *metricspb.MetricsData:
		m.ResourceMetrics = slices.DeleteFunc(m.ResourceMetrics, func(rm *metricspb.ResourceMetrics) bool {
			res := hit(rm.GetResource())
			rm.ScopeMetrics = slices.DeleteFunc(rm.ScopeMetrics, func(sm *metricspb.ScopeMetrics) bool {
				scope := res || hit(sm.GetScope())
				sm.Metrics = slices.DeleteFunc(sm.Metrics, func(mt *metricspb.Metric) bool {
					return dropPoints(mt, scope || metadataExcluded(mt, excludes), hit, &n) == 0
				})
				return len(sm.Metrics) == 0
			})
			return len(rm.ScopeMetrics) == 0
		})
	}
	p.records -= n
	return n
}

// emptied reports whether dropExcluded left msg with no records: it removes what it empties.
func emptied(msg proto.Message) bool {
	switch m := msg.(type) {
	case *logspb.LogsData:
		return len(m.ResourceLogs) == 0
	case *tracepb.TracesData:
		return len(m.ResourceSpans) == 0
	case *metricspb.MetricsData:
		return len(m.ResourceMetrics) == 0
	}
	return false
}

// dropRecords removes every record when all is set, else those hit names, counting them in n.
func dropRecords[T proto.Message](records []T, all bool, hit func(proto.Message) bool, n *int) []T {
	before := len(records)
	records = slices.DeleteFunc(records, func(r T) bool { return all || hit(r) })
	*n += before - len(records)
	return records
}

// dropPoints removes a metric's excluded data points, and reports how many are left.
func dropPoints(m *metricspb.Metric, all bool, hit func(proto.Message) bool, n *int) int {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		d.Sum.DataPoints = dropRecords(d.Sum.GetDataPoints(), all, hit, n)
		return len(d.Sum.DataPoints)
	case *metricspb.Metric_Gauge:
		d.Gauge.DataPoints = dropRecords(d.Gauge.GetDataPoints(), all, hit, n)
		return len(d.Gauge.DataPoints)
	case *metricspb.Metric_Histogram:
		d.Histogram.DataPoints = dropRecords(d.Histogram.GetDataPoints(), all, hit, n)
		return len(d.Histogram.DataPoints)
	case *metricspb.Metric_ExponentialHistogram:
		d.ExponentialHistogram.DataPoints = dropRecords(d.ExponentialHistogram.GetDataPoints(), all, hit, n)
		return len(d.ExponentialHistogram.DataPoints)
	case *metricspb.Metric_Summary:
		d.Summary.DataPoints = dropRecords(d.Summary.GetDataPoints(), all, hit, n)
		return len(d.Summary.DataPoints)
	}
	return 0
}

// metadataExcluded reports whether a metric's own metadata names an excluded file, which
// takes every point of it.
func metadataExcluded(m *metricspb.Metric, excludes func(any) bool) bool {
	return slices.ContainsFunc(m.GetMetadata(), func(kv *commonpb.KeyValue) bool {
		return excludedIn(kv.ProtoReflect(), excludes)
	})
}

func excludedIn(m protoreflect.Message, excludes func(any) bool) bool {
	// protojson leaves an empty key out, and an attribute without one names nothing.
	if kv, ok := m.Interface().(*commonpb.KeyValue); ok && kv.GetKey() != "" {
		if excludes(map[string]any{kv.GetKey(): anyValueJSON(kv.GetValue())}) {
			return true
		}
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsMap():
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				found = excludedIn(mv.Message(), excludes)
				return !found
			})
		case fd.IsList():
			for i, list := 0, v.List(); i < list.Len() && !found; i++ {
				found = excludedIn(list.Get(i).Message(), excludes)
			}
		default:
			found = excludedIn(v.Message(), excludes)
		}
		return !found
	})
	return found
}

// anyValueJSON is v in protojson's shape, the one Policy.Excludes reads.
func anyValueJSON(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return map[string]any{"stringValue": x.StringValue}
	case *commonpb.AnyValue_BoolValue:
		return map[string]any{"boolValue": x.BoolValue}
	case *commonpb.AnyValue_IntValue:
		return map[string]any{"intValue": strconv.FormatInt(x.IntValue, 10)}
	case *commonpb.AnyValue_DoubleValue:
		return map[string]any{"doubleValue": x.DoubleValue}
	case *commonpb.AnyValue_BytesValue:
		return map[string]any{"bytesValue": base64.StdEncoding.EncodeToString(x.BytesValue)}
	case *commonpb.AnyValue_ArrayValue:
		values := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			values = append(values, anyValueJSON(e))
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	case *commonpb.AnyValue_KvlistValue:
		values := make([]any, 0, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			entry := map[string]any{"value": anyValueJSON(kv.GetValue())}
			if kv.GetKey() != "" {
				entry["key"] = kv.GetKey()
			}
			values = append(values, entry)
		}
		return map[string]any{"kvlistValue": map[string]any{"values": values}}
	}
	return map[string]any{}
}
