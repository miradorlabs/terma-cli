package relay

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The heartbeat is one log record naming the machine, with terma's version and what
// started the relay on its resource, timestamped when sent; it goes through HeartbeatSend,
// never the outbox, and a failed beat is not kept. Its terma.relay.heartbeat.reason says why
// it was sent. Every beat carries the counters since the previous one as
// terma.relay.heartbeat.counter.<name> ints, so a relay's beats sum to everything it
// counted; the last, as the relay stops, also says why it stopped.

const (
	// HeartbeatService is the heartbeat's service.name.
	HeartbeatService = "terma-relay"
	// DefaultHeartbeatEvery is the heartbeat period when Options.HeartbeatEvery is 0.
	DefaultHeartbeatEvery = 15 * time.Minute
)

var errNoHeartbeat = errors.New("this relay sends no heartbeat")

// heartbeat sends one beat, with the counters since the last beat that carried them and
// exit, when set, as terma.relay.exit.reason. The counters are taken as it leaves, so two
// beats in flight never report the same ones, and handed back if it fails, for the next.
func (r *Relay) heartbeat(ctx context.Context, reason, exit string) error {
	if r.opts.HeartbeatSend == nil {
		return errNoHeartbeat
	}
	delta := r.stats.takeSinceBeat()
	var attrs []*commonpb.KeyValue
	if exit != "" {
		attrs = append(attrs, &commonpb.KeyValue{Key: semconv.TermaRelayExitReasonKey, Value: strValue(exit)})
	}
	unclassified := 0
	for _, name := range slices.Sorted(maps.Keys(delta)) {
		// What the content policy met unclassified is counted under the attribute's name,
		// which the exporter chose: only the total leaves the machine.
		if strings.HasPrefix(name, unclassifiedPrefix) {
			unclassified += delta[name]
			continue
		}
		attrs = append(attrs, counterAttr(name, delta[name]))
	}
	if unclassified != 0 {
		attrs = append(attrs, counterAttr(unclassifiedPrefix, unclassified))
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := r.opts.HeartbeatSend(ctx, r.heartbeatData(reason, attrs)); err != nil {
		r.stats.untake(delta)
		r.stats.add("heartbeats_failed", 1)
		r.warnf("heartbeat: %v", err)
		return err
	}
	r.stats.add("heartbeats_sent", 1)
	return nil
}

// unclassifiedPrefix starts the counters named after an attribute (Stats.unclassified).
const unclassifiedPrefix = "unclassified"

func counterAttr(name string, n int) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: semconv.TermaRelayHeartbeatCounterKey + "." + name,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(n)}}}
}

// StopHeartbeat is the relay's last beat, sent as it stops: reason exit, exit as
// terma.relay.exit.reason, and the counters since the previous beat. A failed one is lost.
func (r *Relay) StopHeartbeat(ctx context.Context, exit string) error {
	return r.heartbeat(ctx, semconv.TermaRelayHeartbeatReasonExit, exit)
}

func (r *Relay) heartbeatData(reason string, attrs []*commonpb.KeyValue) *logspb.LogsData {
	host, _ := os.Hostname()
	now := uint64(r.opts.Now().UnixNano())
	rec := &logspb.LogRecord{
		TimeUnixNano: now, ObservedTimeUnixNano: now,
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
		EventName: semconv.TermaRelayHeartbeatEvent,
		Body:      strValue(semconv.TermaRelayHeartbeatEvent),
		Attributes: append([]*commonpb.KeyValue{
			{Key: semconv.TermaRelayHeartbeatReasonKey, Value: strValue(reason)},
			{Key: semconv.HostNameKey, Value: strValue(host)},
		}, attrs...),
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: semconv.ServiceNameKey, Value: strValue(HeartbeatService)},
		{Key: semconv.ServiceVersionKey, Value: strValue(r.opts.Version)},
		{Key: semconv.HostNameKey, Value: strValue(host)},
		{Key: semconv.TermaSchemaVersionKey, Value: strValue(semconv.SchemaVersion)},
	}}
	if r.opts.Launch != "" {
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: semconv.TermaRelayLaunchKey, Value: strValue(r.opts.Launch)})
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: HeartbeatService, Version: r.opts.Version}, LogRecords: []*logspb.LogRecord{rec}}}}}}
}
