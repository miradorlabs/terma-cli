package relay

import (
	"context"
	"errors"
	"os"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The heartbeat is one log record naming the machine, with terma's version on its resource,
// timestamped when sent; it goes through HeartbeatSend, never the outbox, and a failed beat
// is not kept. Its terma.relay.heartbeat.reason says why it was sent.

const (
	// HeartbeatService is the heartbeat's service.name.
	HeartbeatService = "terma-relay"
	// DefaultHeartbeatEvery is the heartbeat period when Options.HeartbeatEvery is 0.
	DefaultHeartbeatEvery = 15 * time.Minute
)

var errNoHeartbeat = errors.New("this relay sends no heartbeat")

func (r *Relay) heartbeat(ctx context.Context, reason string) error {
	if r.opts.HeartbeatSend == nil {
		return errNoHeartbeat
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := r.opts.HeartbeatSend(ctx, r.heartbeatData(reason)); err != nil {
		r.stats.add("heartbeats_failed", 1)
		r.warnf("heartbeat: %v", err)
		return err
	}
	r.stats.add("heartbeats_sent", 1)
	return nil
}

func (r *Relay) heartbeatData(reason string) *logspb.LogsData {
	host, _ := os.Hostname()
	now := uint64(r.opts.Now().UnixNano())
	rec := &logspb.LogRecord{
		TimeUnixNano: now, ObservedTimeUnixNano: now,
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
		EventName: semconv.TermaRelayHeartbeatEvent,
		Body:      strValue(semconv.TermaRelayHeartbeatEvent),
		Attributes: []*commonpb.KeyValue{
			{Key: semconv.TermaRelayHeartbeatReasonKey, Value: strValue(reason)},
			{Key: semconv.HostNameKey, Value: strValue(host)},
		},
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: semconv.ServiceNameKey, Value: strValue(HeartbeatService)},
		{Key: semconv.ServiceVersionKey, Value: strValue(r.opts.Version)},
		{Key: semconv.HostNameKey, Value: strValue(host)},
		{Key: semconv.TermaSchemaVersionKey, Value: strValue(semconv.SchemaVersion)},
	}}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: HeartbeatService, Version: r.opts.Version}, LogRecords: []*logspb.LogRecord{rec}}}}}}
}
