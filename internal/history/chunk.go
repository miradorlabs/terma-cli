package history

import (
	"fmt"
	"slices"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// MaxRequestBytes caps each OTLP /v1/logs request body at 3.5 MiB so requests stay safely
// beneath the ingest gateway's 4 MiB limit.
const MaxRequestBytes = 3584 << 10

// Chunk is one marshaled OTLP LogsData protobuf payload ready to send.
type Chunk struct {
	Body    []byte
	Records int
}

// PrepareSegment applies the destination team's content policy and resource stamps to one
// transcript segment, preserving terma.session.historical=true on every resource and log
// record. Native logs pass through the relay's ContentFilter; supplemental terma-scoped
// records (such as assistant replies) are included only when the team policy allows prompts.
func PrepareSegment(seg agents.HistorySegment, filter *relay.ContentFilter, pol config.Policy, stamp relay.StampOptions) (*logspb.LogsData, int) {
	prompts, toolContent := pol.Content()
	stamp.IncludeToolContent = toolContent
	out := &logspb.LogsData{}
	total := 0

	if seg.NativeLogs != nil {
		native := proto.CloneOf(seg.NativeLogs)
		ensureHistorical(native)
		if filter != nil {
			filter.FilterLogs(native, prompts, toolContent)
		}
		relay.StampLogs(native, stamp)
		ensureHistorical(native)
		if n := countLogs(native); n > 0 {
			out.ResourceLogs = append(out.ResourceLogs, native.ResourceLogs...)
			total += n
		}
	}

	if seg.SupplementalLogs != nil && prompts {
		supp := proto.CloneOf(seg.SupplementalLogs)
		ensureHistorical(supp)
		stampSupplemental(supp, stamp)
		ensureHistorical(supp)
		if n := countLogs(supp); n > 0 {
			out.ResourceLogs = append(out.ResourceLogs, supp.ResourceLogs...)
			total += n
		}
	}

	if total == 0 {
		return nil, 0
	}
	return out, total
}

// stampSupplemental applies terma resource metadata and checkout stamps to supplemental
// terma-scoped log batches, stripping local checkout paths when toolContent is withheld.
func stampSupplemental(msg *logspb.LogsData, opts relay.StampOptions) {
	relay.StampLogs(msg, opts)
	for _, rl := range msg.GetResourceLogs() {
		if rl.Resource == nil {
			rl.Resource = &resourcepb.Resource{}
		}
		if opts.Version != "" {
			setAttr(rl.Resource, semconv.ServiceVersionKey, opts.Version)
		}
		setAttr(rl.Resource, semconv.TermaSchemaVersionKey, semconv.SchemaVersion)
		for _, sl := range rl.GetScopeLogs() {
			if sl.Scope != nil && opts.Version != "" {
				sl.Scope.Version = opts.Version
			}
			if !opts.IncludeToolContent {
				for _, lr := range sl.GetLogRecords() {
					lr.Attributes = slices.DeleteFunc(lr.Attributes, func(kv *commonpb.KeyValue) bool {
						return kv.GetKey() == semconv.TermaRepositoryRootKey ||
							kv.GetKey() == semconv.TermaWorkingDirectoryKey
					})
				}
			}
		}
	}
}

func setAttr(res *resourcepb.Resource, key, val string) {
	v := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}}
	for _, kv := range res.Attributes {
		if kv.GetKey() == key {
			kv.Value = v
			return
		}
	}
	res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: key, Value: v})
}

func ensureHistorical(msg *logspb.LogsData) {
	boolTrue := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
	setBool := func(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
		for _, kv := range attrs {
			if kv.GetKey() == semconv.TermaSessionHistoricalKey {
				kv.Value = proto.CloneOf(boolTrue)
				return attrs
			}
		}
		return append(attrs, &commonpb.KeyValue{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)})
	}
	for _, rl := range msg.GetResourceLogs() {
		if rl.Resource == nil {
			rl.Resource = &resourcepb.Resource{}
		}
		rl.Resource.Attributes = setBool(rl.Resource.Attributes)
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				lr.Attributes = setBool(lr.Attributes)
			}
		}
	}
}

func countLogs(msg *logspb.LogsData) int {
	n := 0
	for _, rl := range msg.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			n += len(sl.GetLogRecords())
		}
	}
	return n
}

// FilterMixedLogs removes any log record whose timestamp falls at or after a Mixed cutoff
// recorded in snap or derived from transcript tool reach (reachMixedAt when hasReachMixed),
// drops records newer than maxRecordTime when resuming a transcript modified after an
// interrupted upload, or drops every record when the ledger is unreadable (failing closed).
func FilterMixedLogs(msg *logspb.LogsData, snap runs.Snapshot, lookup func(id string) runs.Snapshot, reachMixedAt time.Time, hasReachMixed bool, maxRecordTime time.Time) (kept, dropped int) {
	if msg == nil {
		return 0, 0
	}
	isMixed := func(lr *logspb.LogRecord) bool {
		t := logRecordTime(lr)
		if !maxRecordTime.IsZero() && !t.IsZero() && t.After(maxRecordTime) {
			return true
		}
		if hasReachMixed && (reachMixedAt.IsZero() || t.IsZero() || !t.Before(reachMixedAt)) {
			return true
		}
		if mixedAt, mixed := snap.MixedAt(t); mixed && (mixedAt.IsZero() || t.IsZero() || !t.Before(mixedAt)) {
			return true
		}
		if lookup != nil {
			if agentID := logRecordAttrString(lr, "agent_id"); agentID != "" {
				sub := lookup(agentID)
				if mixedAt, mixed := sub.MixedAt(t); mixed && (mixedAt.IsZero() || t.IsZero() || !t.Before(mixedAt)) {
					return true
				}
			}
		}
		return false
	}
	var keptRL []*logspb.ResourceLogs
	for _, rl := range msg.GetResourceLogs() {
		var keptSL []*logspb.ScopeLogs
		for _, sl := range rl.GetScopeLogs() {
			var keptLR []*logspb.LogRecord
			for _, lr := range sl.GetLogRecords() {
				if isMixed(lr) {
					dropped++
					continue
				}
				keptLR = append(keptLR, lr)
				kept++
			}
			if len(keptLR) > 0 {
				sl.LogRecords = keptLR
				keptSL = append(keptSL, sl)
			}
		}
		if len(keptSL) > 0 {
			rl.ScopeLogs = keptSL
			keptRL = append(keptRL, rl)
		}
	}
	msg.ResourceLogs = keptRL
	return kept, dropped
}

func logRecordTime(lr *logspb.LogRecord) time.Time {
	if ns := lr.GetTimeUnixNano(); ns > 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	if ns := lr.GetObservedTimeUnixNano(); ns > 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	return time.Time{}
}

func logRecordAttrString(lr *logspb.LogRecord, key string) string {
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// ChunkLogs marshals prepared OTLP log batches into one or more protobuf request bodies,
// each strictly at most maxBytes bytes. If a single log record cannot fit within maxBytes
// it returns a *PermanentError so the session is recorded as failed rather than sent oversized.
func ChunkLogs(batches []*logspb.LogsData, maxBytes int) ([]Chunk, error) {
	if maxBytes <= 0 {
		maxBytes = MaxRequestBytes
	}
	var chunks []Chunk
	cur := &logspb.LogsData{}
	curRecords := 0
	estBytes := 0

	flush := func() error {
		if curRecords == 0 {
			return nil
		}
		wire, err := proto.Marshal(cur)
		if err != nil {
			return fmt.Errorf("marshal OTLP logs: %w", err)
		}
		if len(wire) > maxBytes {
			return &PermanentError{Reason: fmt.Sprintf("marshaled batch (%d bytes) exceeds %d-byte limit", len(wire), maxBytes)}
		}
		chunks = append(chunks, Chunk{Body: wire, Records: curRecords})
		cur = &logspb.LogsData{}
		curRecords = 0
		estBytes = 0
		return nil
	}

	for _, batch := range batches {
		for _, rl := range batch.GetResourceLogs() {
			resOverhead := proto.Size(rl.Resource) + len(rl.SchemaUrl) + 32
			for _, sl := range rl.GetScopeLogs() {
				if len(sl.GetLogRecords()) == 0 {
					continue
				}
				scopeOverhead := resOverhead + proto.Size(sl.Scope) + len(sl.SchemaUrl) + 32
				var activeSL *logspb.ScopeLogs
				openScope := func() {
					activeSL = &logspb.ScopeLogs{Scope: proto.CloneOf(sl.Scope), SchemaUrl: sl.SchemaUrl}
					cur.ResourceLogs = append(cur.ResourceLogs, &logspb.ResourceLogs{
						Resource:  proto.CloneOf(rl.Resource),
						ScopeLogs: []*logspb.ScopeLogs{activeSL},
						SchemaUrl: rl.SchemaUrl,
					})
					estBytes += scopeOverhead
				}
				for _, lr := range sl.GetLogRecords() {
					if activeSL == nil {
						openScope()
					}
					recSize := proto.Size(lr) + 16
					activeSL.LogRecords = append(activeSL.LogRecords, lr)
					estBytes += recSize
					// Only run exact proto.Size(cur) when our conservative upper-bound
					// estimate crosses maxBytes, resyncing estBytes when cur still fits.
					if estBytes <= maxBytes {
						curRecords++
						continue
					}
					if exact := proto.Size(cur); exact <= maxBytes {
						estBytes = exact
						curRecords++
						continue
					}
					// Undo append and flush current batch if it already has records.
					activeSL.LogRecords = activeSL.LogRecords[:len(activeSL.LogRecords)-1]
					if len(activeSL.LogRecords) == 0 {
						cur.ResourceLogs = cur.ResourceLogs[:len(cur.ResourceLogs)-1]
					}
					if curRecords == 0 {
						return nil, &PermanentError{
							Reason: fmt.Sprintf("single log record exceeds %d-byte request limit", maxBytes),
						}
					}
					if err := flush(); err != nil {
						return nil, err
					}
					openScope()
					activeSL.LogRecords = append(activeSL.LogRecords, lr)
					estBytes += recSize
					if estBytes > maxBytes && proto.Size(cur) > maxBytes {
						return nil, &PermanentError{
							Reason: fmt.Sprintf("single log record exceeds %d-byte request limit", maxBytes),
						}
					}
					curRecords++
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return chunks, nil
}
