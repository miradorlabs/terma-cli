package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// A harness's own telemetry to its vendor, from the requests the recording proxy saw: Claude
// Code's event_logging batches, Gemini CLI's Clearcut log, Muse's OTLP logs and traces. Each event is named where it says
// its event_name (an event's own object, or the JSON a field holds as a string, as Clearcut's
// source_extension_json): what the build reports, by event, how often a run reported it, and
// the fields it carries. Never their values: the names say what is reported, the values are
// the run's (its device, its session).

// eventField is the name an event is known by.
const eventField = "event_name"

// vendorTelemetry is what the egress said of the build's telemetry, by endpoint ("POST
// api.anthropic.com/api/event_logging/v2/batch"): each event's count and fields.
func vendorTelemetry(egress []e2e.ProviderRequest) map[string]any {
	out := map[string]any{}
	for _, r := range egress {
		events := map[string]map[string]bool{}
		counts := map[string]int{}
		var b any
		if len(r.Body) > 0 && json.Unmarshal(r.Body, &b) == nil {
			collectEvents(b, events, counts)
		} else {
			collectOTLP(r, events, counts)
		}
		if len(events) == 0 {
			continue
		}
		key := r.Method + " " + r.Host + r.Path
		ep, _ := out[key].(map[string]any)
		if ep == nil {
			ep = map[string]any{"events": map[string]any{}}
			out[key] = ep
		}
		evs := ep["events"].(map[string]any)
		for name, fields := range events {
			e, _ := evs[name].(map[string]any)
			if e == nil {
				e = map[string]any{"count": 0, "fields": []string{}}
				evs[name] = e
			}
			e["count"] = e["count"].(int) + counts[name]
			have := map[string]bool{}
			for _, f := range e["fields"].([]string) {
				have[f] = true
			}
			for f := range fields {
				have[f] = true
			}
			e["fields"] = sortedKeys(have)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectEvents finds every event in v: an object that names its event_name, its fields two
// keys deep; a string holding JSON is read as what it holds.
func collectEvents(v any, events map[string]map[string]bool, counts map[string]int) {
	switch x := v.(type) {
	case map[string]any:
		if name, ok := x[eventField].(string); ok && name != "" {
			if events[name] == nil {
				events[name] = map[string]bool{}
			}
			counts[name]++
			for k, val := range x {
				events[name][k] = true
				if m, ok := val.(map[string]any); ok {
					for sk := range m {
						events[name][k+"."+sk] = true
					}
				}
			}
			return
		}
		for _, val := range x {
			collectEvents(val, events, counts)
		}
	case []any:
		for _, val := range x {
			collectEvents(val, events, counts)
		}
	case string:
		if t := strings.TrimSpace(x); strings.HasPrefix(t, "{") && strings.Contains(t, eventField) {
			var inner any
			if json.Unmarshal([]byte(t), &inner) == nil {
				collectEvents(inner, events, counts)
			}
		}
	}
}

// sortedEventNames are an endpoint's events, for tests.
func sortedEventNames(ep map[string]any) []string {
	evs, _ := ep["events"].(map[string]any)
	out := make([]string, 0, len(evs))
	for k := range evs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectOTLP finds the events of an OTLP/HTTP protobuf export r carried (Muse's own
// telemetry): a log record is the event its event_name attribute names (else its own event
// name), a span the event its name is, each with its attributes' keys, and the resource's as
// resource.<key>.
func collectOTLP(r e2e.ProviderRequest, events map[string]map[string]bool, counts map[string]int) {
	raw, ok := strings.CutPrefix(r.Text, "base64:")
	if !ok || !strings.Contains(strings.Join(r.Header["content-type"], ","), "protobuf") {
		return
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return
	}
	add := func(name string, resource *resourcepb.Resource, attrs []*commonpb.KeyValue) {
		if name == "" {
			return
		}
		if events[name] == nil {
			events[name] = map[string]bool{}
		}
		counts[name]++
		for _, kv := range resource.GetAttributes() {
			events[name]["resource."+kv.GetKey()] = true
		}
		for _, kv := range attrs {
			events[name][kv.GetKey()] = true
		}
	}
	switch {
	case strings.HasSuffix(r.Path, "/logs"):
		var req collogspb.ExportLogsServiceRequest
		if proto.Unmarshal(data, &req) != nil {
			return
		}
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					name := lr.GetEventName()
					for _, kv := range lr.GetAttributes() {
						if kv.GetKey() == eventField && kv.GetValue().GetStringValue() != "" {
							name = kv.GetValue().GetStringValue()
						}
					}
					add(name, rl.GetResource(), lr.GetAttributes())
				}
			}
		}
	case strings.HasSuffix(r.Path, "/traces"):
		var req coltracepb.ExportTraceServiceRequest
		if proto.Unmarshal(data, &req) != nil {
			return
		}
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					add("span:"+sp.GetName(), rs.GetResource(), sp.GetAttributes())
				}
			}
		}
	}
}
