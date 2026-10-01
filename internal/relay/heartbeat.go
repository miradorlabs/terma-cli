package relay

import (
	"context"
	"errors"
	"os"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// The heartbeat is one log record naming terma's version and the machine, timestamped when
// sent; it goes through HeartbeatSend, never the outbox, and a failed beat is not kept.

const (
	// HeartbeatEvent names the heartbeat's log record.
	HeartbeatEvent = "terma.relay.heartbeat"
	// HeartbeatService is the heartbeat's service.name.
	HeartbeatService = "terma-relay"
	// DefaultHeartbeatEvery is the heartbeat period when Options.HeartbeatEvery is 0.
	DefaultHeartbeatEvery = 15 * time.Minute
)

// HeartbeatReasonAttr says why a beat was sent; HeartbeatSetup is the platform's "installed and working".
const (
	HeartbeatReasonAttr = "terma.heartbeat.reason"
	HeartbeatStart      = "start"
	HeartbeatInterval   = "interval"
	HeartbeatSetup      = "setup"
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
		if r.opts.Logf != nil {
			r.opts.Logf("heartbeat: %v", err)
		}
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
		Body: strValue(HeartbeatEvent),
		Attributes: []*commonpb.KeyValue{
			{Key: "event.name", Value: strValue(HeartbeatEvent)},
			{Key: HeartbeatReasonAttr, Value: strValue(reason)},
			{Key: "terma.version", Value: strValue(r.opts.Version)},
			{Key: "host.name", Value: strValue(host)},
		},
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "service.name", Value: strValue(HeartbeatService)},
		{Key: "service.version", Value: strValue(r.opts.Version)},
		{Key: "host.name", Value: strValue(host)},
	}}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: HeartbeatService, Version: r.opts.Version}, LogRecords: []*logspb.LogRecord{rec}}}}}}
}
