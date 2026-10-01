package relay

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The heartbeat describes the machine, not a project, so it carries no project and goes
// with the developer's credential, never a project key or the outbox. A failed beat is not
// kept: its counters are cumulative.

const (
	// HeartbeatEvent names the heartbeat's log record.
	HeartbeatEvent = "terma.relay.heartbeat"
	// HeartbeatService is the heartbeat's service.name.
	HeartbeatService = "terma-relay"
	// DefaultHeartbeatEvery is the heartbeat period when Options.HeartbeatEvery is 0.
	DefaultHeartbeatEvery = 15 * time.Minute
)

const maxAgentVersions = 32

func (r *Relay) noteDelivery(p *part) {
	name, version := resourceService(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastDelivery = r.opts.Now()
	if name != "" && (len(r.agentVersions) < maxAgentVersions || r.agentVersions[name] != "") {
		r.agentVersions[name] = version
	}
}

func resourceService(p *part) (name, version string) {
	var res *resourcepb.Resource
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		if rl := m.GetResourceLogs(); len(rl) > 0 {
			res = rl[0].GetResource()
		}
	case *tracepb.TracesData:
		if rs := m.GetResourceSpans(); len(rs) > 0 {
			res = rs[0].GetResource()
		}
	case *metricspb.MetricsData:
		if rm := m.GetResourceMetrics(); len(rm) > 0 {
			res = rm[0].GetResource()
		}
	}
	for _, kv := range res.GetAttributes() {
		switch kv.GetKey() {
		case "service.name":
			name = kv.GetValue().GetStringValue()
		case "service.version":
			version = kv.GetValue().GetStringValue()
		}
	}
	return name, version
}

// HeartbeatReasonAttr says why a beat was sent; HeartbeatSetup is the platform's "installed and working".
const (
	HeartbeatReasonAttr = "terma.heartbeat.reason"
	HeartbeatStart      = "start"
	HeartbeatInterval   = "interval"
	HeartbeatSetup      = "setup"
)

var errNoHeartbeat = errors.New("this relay sends no heartbeat")

func (r *Relay) heartbeat(ctx context.Context, reason string) error {
	if r.opts.HeartbeatInfo == nil || r.opts.HeartbeatSend == nil {
		return errNoHeartbeat
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := r.opts.HeartbeatSend(ctx, r.heartbeatData(reason)); err != nil {
		r.stats.add("heartbeats_failed", 1)
		if r.opts.Logf != nil {
			r.opts.Logf("heartbeat: %v", err)
		}
		return err
	}
	r.stats.add("heartbeats_sent", 1)
	return nil
}

func (r *Relay) heartbeatData(reason string) *logspb.LogsData {
	var attrs []*commonpb.KeyValue
	add := func(k string, v *commonpb.AnyValue) { attrs = append(attrs, &commonpb.KeyValue{Key: k, Value: v}) }
	add("event.name", strValue(HeartbeatEvent))
	add(HeartbeatReasonAttr, strValue(reason))
	info := r.opts.HeartbeatInfo()
	for _, k := range sortedKeys(info) {
		add(k, anyOf(info[k]))
	}
	r.mu.Lock()
	agents := make(map[string]any, len(r.agentVersions))
	for k, v := range r.agentVersions {
		agents[k] = v
	}
	last, held := r.lastDelivery, r.heldN
	r.mu.Unlock()
	for _, n := range sortedKeys(agents) {
		add("relay.agent."+n+".version", strValue(agents[n].(string)))
	}
	if !last.IsZero() {
		add("relay.last_delivery_at", strValue(last.UTC().Format(time.RFC3339)))
	}
	snap := r.stats.Snapshot()
	add("relay.started_at", strValue(snap.Since.UTC().Format(time.RFC3339)))
	add("relay.uptime_s", intValue(int64(r.opts.Now().Sub(snap.Since).Seconds())))
	unclassified := 0
	for _, k := range snap.Keys() {
		// Unclassified keys are named in the local stats; the heartbeat says how many.
		if strings.HasPrefix(k, "unclassified.") {
			unclassified++
			continue
		}
		add("relay.count."+k, intValue(int64(snap.Counters[k])))
	}
	add("relay.unclassified_keys", intValue(int64(unclassified)))
	parts, records := 0, 0
	for _, q := range Backlog(r.opts.Dir) {
		parts += q.Parts
		records += q.Records
	}
	add("relay.outbox.parts", intValue(int64(parts)))
	add("relay.outbox.records", intValue(int64(records)))
	add("relay.held.records", intValue(int64(held)))

	now := uint64(r.opts.Now().UnixNano())
	rec := &logspb.LogRecord{
		TimeUnixNano: now, ObservedTimeUnixNano: now,
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
		Body: strValue(HeartbeatEvent), Attributes: attrs,
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "service.name", Value: strValue(HeartbeatService)},
		{Key: "service.version", Value: strValue(r.opts.Version)},
	}}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: HeartbeatService, Version: r.opts.Version}, LogRecords: []*logspb.LogRecord{rec}}}}}}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func intValue(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

func anyOf(v any) *commonpb.AnyValue {
	switch t := v.(type) {
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: t}}
	case int:
		return intValue(int64(t))
	case int64:
		return intValue(t)
	case []string:
		vals := make([]*commonpb.AnyValue, len(t))
		for i, s := range t {
			vals[i] = strValue(s)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	case string:
		return strValue(t)
	}
	return strValue("")
}
