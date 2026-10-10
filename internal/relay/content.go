package relay

import (
	"slices"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// withhold applies a project's content policy to a part in place and returns how many records it changed.
func (ru *rules) withhold(p *part, prompts, toolContent bool, unclassified map[string]int) int {
	if prompts && toolContent {
		return 0
	}
	changed := 0
	applyIn := func(attrs []*commonpb.KeyValue, toolEvent bool) []*commonpb.KeyValue {
		out, did := ru.withholdAttrs(attrs, prompts, toolContent, toolEvent, unclassified)
		if did {
			changed++
		}
		return out
	}
	apply := func(attrs []*commonpb.KeyValue) []*commonpb.KeyValue { return applyIn(attrs, false) }
	resource := func(r *resourcepb.Resource) {
		if r == nil {
			return
		}
		kept := r.Attributes[:0]
		for _, kv := range r.GetAttributes() {
			switch key := kv.GetKey(); {
			case contains(ru.resourcePromptFields, key):
				if !prompts {
					changed++
					continue
				}
			case key == semconv.TermaRepositoryRootKey, key == semconv.TermaWorkingDirectoryKey:
				// The relay's own stamps: local paths, which leave only with tool content.
				if !toolContent {
					changed++
					continue
				}
			case !ru.safeKey(key):
				unclassified["resource/"+key]++
				changed++
				continue
			}
			kept = append(kept, kv)
		}
		r.Attributes = kept
	}
	exemplars := func(values []*metricspb.Exemplar) {
		for _, value := range values {
			value.FilteredAttributes = apply(value.FilteredAttributes)
		}
	}
	switch m := p.msg.(type) {
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			resource(rm.GetResource())
			for _, sm := range rm.GetScopeMetrics() {
				if sm.Scope != nil {
					sm.Scope.Attributes = apply(sm.Scope.Attributes)
				}
				for _, mt := range sm.GetMetrics() {
					for _, pt := range mt.GetSum().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetGauge().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetHistogram().GetDataPoints() {
						pt.Attributes = apply(pt.GetAttributes())
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetExponentialHistogram().GetDataPoints() {
						pt.Attributes = apply(pt.Attributes)
						exemplars(pt.Exemplars)
					}
					for _, pt := range mt.GetSummary().GetDataPoints() {
						pt.Attributes = apply(pt.Attributes)
					}
				}
			}
		}
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			resource(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				if sl.Scope != nil {
					sl.Scope.Attributes = apply(sl.Scope.Attributes)
				}
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = apply(lr.GetAttributes())
					event := attrString(lr.GetAttributes(), "event.name")
					_, plain := lr.GetBody().GetValue().(*commonpb.AnyValue_StringValue)
					body := lr.GetBody().GetStringValue()
					switch {
					case lr.GetBody().GetValue() == nil || plain && body == "":
					case contains(ru.promptBodyEvents, event):
						// The body is what was said: it follows the prompt policy alone.
						if !prompts {
							lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}
							changed++
						}
					case !plain || !ru.bodyNamesItsEvent(body, event):
						// A body is free text: kept only when it just names its event.
						lr.Body = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}
						changed++
					}
				}
			}
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			resource(rs.GetResource())
			for _, ss := range rs.GetScopeSpans() {
				if ss.Scope != nil {
					ss.Scope.Attributes = apply(ss.Scope.Attributes)
				}
				for _, sp := range ss.GetSpans() {
					// Provider errors can repeat either prompts or tool input/output.
					if sp.Status != nil && sp.Status.Message != "" {
						sp.Status.Message = ""
						changed++
					}
					for _, link := range sp.Links {
						link.Attributes = apply(link.Attributes)
					}
					sp.Attributes = apply(sp.GetAttributes())
					events := sp.GetEvents()[:0]
					for _, ev := range sp.GetEvents() {
						toolEvent := contains(ru.toolContentEvents, ev.GetName())
						if !toolContent && toolEvent {
							changed++
							continue
						}
						// An unnamed key in a tool-content event is the tool's own output.
						ev.Attributes = applyIn(ev.GetAttributes(), toolEvent)
						events = append(events, ev)
					}
					sp.Events = events
				}
			}
		}
	}
	return changed
}

// withholdAttrs applies the content policy to attrs; in a toolEvent an unclassified key is kept.
func (ru *rules) withholdAttrs(attrs []*commonpb.KeyValue, prompts, toolContent, toolEvent bool, unclassified map[string]int) ([]*commonpb.KeyValue, bool) {
	marker, principal := ru.marker(attrs), ru.principal(attrs)
	changed := false
	out := attrs[:0]
	for _, kv := range attrs {
		key := kv.GetKey()
		switch {
		case !toolContent && contains(ru.toolContentFields, key):
			changed = true
			continue
		case !prompts && (contains(ru.promptDropFields, key) || contains(ru.resourcePromptFields, key)):
			// A process's arguments are withheld on a record as on its resource.
			changed = true
			continue
		case !prompts && contains(ru.promptFields, key) && kv.GetValue().GetStringValue() != marker:
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}}
			changed = true
		case !toolEvent && !ru.contentKey(key) && !ru.safeKey(key) && !scalarNonText(kv.GetValue()):
			unclassified[key]++
			changed = true
			continue
		}
		out = append(out, kv)
	}
	if principal != nil {
		out, changed = append(out, principal), true
	}
	return out, changed
}

// scalarNonText reports whether v is a number or boolean, which cannot carry what was said
// whatever its key; agents send many as strings ("3", "true"), which count the same.
func scalarNonText(v *commonpb.AnyValue) bool {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue, *commonpb.AnyValue_DoubleValue, *commonpb.AnyValue_BoolValue:
		return true
	case *commonpb.AnyValue_StringValue:
		return numericOrBool(x.StringValue)
	}
	return false
}

func numericOrBool(s string) bool {
	if s == "true" || s == "false" {
		return true
	}
	if s == "" || len(s) > 32 {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil && !strings.ContainsAny(s, "xXpPiInN_")
}

func (ru *rules) bodyNamesItsEvent(body, event string) bool {
	if body == event {
		return true
	}
	return event != "" && slices.ContainsFunc(ru.bodyPrefixes, func(prefix string) bool { return body == prefix+event })
}

func attrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func contains(set []string, s string) bool {
	return slices.Contains(set, s)
}
