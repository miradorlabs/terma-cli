package history

import (
	"fmt"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func ensureHistoricalTraces(msg *tracepb.TracesData) {
	for _, rs := range msg.GetResourceSpans() {
		if rs.Resource == nil {
			rs.Resource = &resourcepb.Resource{}
		}
		rs.Resource.Attributes = setHistoricalAttr(rs.Resource.Attributes)
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				sp.Attributes = setHistoricalAttr(sp.Attributes)
			}
		}
	}
}

func countTraces(msg *tracepb.TracesData) int {
	n := 0
	for _, rs := range msg.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return n
}

// filterSupplementalLogs keeps only supplemental terma-scoped log records allowed under the
// destination team's content policy: prompts/replies/titles require prompts=true; structural
// metadata (session starts, file touches) leave under any policy; unknown kinds fail closed.
func filterSupplementalLogs(msg *logspb.LogsData, prompts, toolContent bool) {
	allowed := func(lr *logspb.LogRecord) bool {
		switch lr.GetEventName() {
		case semconv.TermaAssistantMessageEvent, semconv.TermaSessionTitleEvent:
			return prompts
		case semconv.TermaSessionStartEvent, semconv.TermaFilesTouchedEvent:
			return true
		default:
			return prompts && toolContent
		}
	}
	var keptRL []*logspb.ResourceLogs
	for _, rl := range msg.GetResourceLogs() {
		var keptSL []*logspb.ScopeLogs
		for _, sl := range rl.GetScopeLogs() {
			var keptLR []*logspb.LogRecord
			for _, lr := range sl.GetLogRecords() {
				if allowed(lr) {
					keptLR = append(keptLR, lr)
				}
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
}

// FilterMixedTraces removes any trace span whose [start, end] interval falls at or after a
// Mixed cutoff recorded in snap or derived from transcript tool reach (reachMixedAt when
// hasReachMixed), drops spans newer than maxRecordTime when resuming a modified transcript,
// or drops every span when the ledger is unreadable (failing closed). dropped counts Mixed
// drops only.
func FilterMixedTraces(msg *tracepb.TracesData, snap runs.Snapshot, lookup func(id string) runs.Snapshot, reachMixedAt time.Time, hasReachMixed bool, maxRecordTime time.Time) (kept, dropped int) {
	if msg == nil {
		return 0, 0
	}
	spanBounds := func(sp *tracepb.Span) (start, end time.Time) {
		start = spanStartTime(sp)
		end = spanEndTime(sp)
		if start.IsZero() && end.IsZero() {
			return time.Time{}, time.Time{}
		}
		if start.IsZero() {
			start = end
		}
		if end.IsZero() || end.Before(start) {
			end = start
		}
		return start, end
	}
	later := func(start, end time.Time) bool {
		return !maxRecordTime.IsZero() && ((!start.IsZero() && start.After(maxRecordTime)) || (!end.IsZero() && end.After(maxRecordTime)))
	}
	crossesSnap := func(s runs.Snapshot, start, end time.Time) bool {
		mixedAt, mixed := s.MixedInterval(start, end)
		return mixed && (mixedAt.IsZero() || start.IsZero() || !start.Before(mixedAt) || !end.Before(mixedAt))
	}
	isMixed := func(sp *tracepb.Span, start, end time.Time) bool {
		if hasReachMixed && (reachMixedAt.IsZero() || start.IsZero() || !start.Before(reachMixedAt) || !end.Before(reachMixedAt)) {
			return true
		}
		if crossesSnap(snap, start, end) {
			return true
		}
		if lookup != nil {
			for _, attrKey := range []string{"gen_ai.conversation.id", "agent_id"} {
				if id := spanAttrString(sp, attrKey); id != "" && crossesSnap(lookup(id), start, end) {
					return true
				}
			}
		}
		return false
	}
	var keptRS []*tracepb.ResourceSpans
	for _, rs := range msg.GetResourceSpans() {
		var keptSS []*tracepb.ScopeSpans
		for _, ss := range rs.GetScopeSpans() {
			var keptSpans []*tracepb.Span
			for _, sp := range ss.GetSpans() {
				start, end := spanBounds(sp)
				if later(start, end) {
					continue
				}
				if isMixed(sp, start, end) {
					dropped++
					continue
				}
				keptSpans = append(keptSpans, sp)
				kept++
			}
			if len(keptSpans) > 0 {
				ss.Spans = keptSpans
				keptSS = append(keptSS, ss)
			}
		}
		if len(keptSS) > 0 {
			rs.ScopeSpans = keptSS
			keptRS = append(keptRS, rs)
		}
	}
	msg.ResourceSpans = keptRS
	return kept, dropped
}

func spanStartTime(sp *tracepb.Span) time.Time {
	if ns := sp.GetStartTimeUnixNano(); ns > 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	return time.Time{}
}

func spanEndTime(sp *tracepb.Span) time.Time {
	if ns := sp.GetEndTimeUnixNano(); ns > 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	return time.Time{}
}

func spanAttrString(sp *tracepb.Span, key string) string {
	for _, kv := range sp.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// AddTraces appends one prepared TracesData batch, flushing completed /v1/traces chunks
// whenever adding a span would take a non-empty chunk past target, or past maxBytes.
// Returns *PermanentError if a single span cannot fit in maxBytes.
func (w *ChunkWriter) AddTraces(batch *tracepb.TracesData) error {
	if batch == nil {
		return nil
	}
	if w.traces == nil {
		w.traces = newTraceChunker(w.maxBytes, w.target, w.onChunk)
	}
	return w.traces.add(batch)
}

// AddPrepared appends both traces and logs from a PreparedSegment into w.
func (w *ChunkWriter) AddPrepared(seg PreparedSegment) error {
	if seg.Traces != nil {
		if err := w.AddTraces(seg.Traces); err != nil {
			return err
		}
	}
	if seg.Logs != nil {
		if err := w.Add(seg.Logs); err != nil {
			return err
		}
	}
	return nil
}

type traceChunker struct {
	maxBytes int
	target   int
	onChunk  func(Chunk) error
	cur      *tracepb.TracesData
	curSpans int
	estBytes int
}

func newTraceChunker(maxBytes, target int, onChunk func(Chunk) error) *traceChunker {
	return &traceChunker{
		maxBytes: maxBytes,
		target:   target,
		onChunk:  onChunk,
		cur:      &tracepb.TracesData{},
	}
}

func (c *traceChunker) add(batch *tracepb.TracesData) error {
	for _, rs := range batch.GetResourceSpans() {
		resOverhead := proto.Size(rs.Resource) + len(rs.SchemaUrl) + 32
		for _, ss := range rs.GetScopeSpans() {
			if len(ss.GetSpans()) == 0 {
				continue
			}
			scopeOverhead := resOverhead + proto.Size(ss.Scope) + len(ss.SchemaUrl) + 32
			var activeSS *tracepb.ScopeSpans
			openScope := func() {
				activeSS = &tracepb.ScopeSpans{Scope: proto.CloneOf(ss.Scope), SchemaUrl: ss.SchemaUrl}
				c.cur.ResourceSpans = append(c.cur.ResourceSpans, &tracepb.ResourceSpans{
					Resource:   proto.CloneOf(rs.Resource),
					ScopeSpans: []*tracepb.ScopeSpans{activeSS},
					SchemaUrl:  rs.SchemaUrl,
				})
				c.estBytes += scopeOverhead
			}
			for _, sp := range ss.GetSpans() {
				spanSize := proto.Size(sp) + 16
				grow := spanSize
				if activeSS == nil {
					grow += scopeOverhead
				}
				if c.target < c.maxBytes && c.curSpans > 0 && c.estBytes+grow > c.target {
					if err := c.flush(); err != nil {
						return err
					}
					activeSS = nil
				}
				if activeSS == nil {
					openScope()
				}
				activeSS.Spans = append(activeSS.Spans, sp)
				c.estBytes += spanSize
				if c.estBytes <= c.maxBytes {
					c.curSpans++
					continue
				}
				if exact := proto.Size(c.cur); exact <= c.maxBytes {
					c.estBytes = exact
					c.curSpans++
					continue
				}
				activeSS.Spans = activeSS.Spans[:len(activeSS.Spans)-1]
				if len(activeSS.Spans) == 0 {
					c.cur.ResourceSpans = c.cur.ResourceSpans[:len(c.cur.ResourceSpans)-1]
				}
				if c.curSpans == 0 {
					return &PermanentError{
						Reason: fmt.Sprintf("single trace span exceeds %d-byte request limit", c.maxBytes),
					}
				}
				if err := c.flush(); err != nil {
					return err
				}
				openScope()
				activeSS.Spans = append(activeSS.Spans, sp)
				c.estBytes += spanSize
				if c.estBytes > c.maxBytes && proto.Size(c.cur) > c.maxBytes {
					return &PermanentError{
						Reason: fmt.Sprintf("single trace span exceeds %d-byte request limit", c.maxBytes),
					}
				}
				c.curSpans++
			}
		}
	}
	return nil
}

func (c *traceChunker) flush() error {
	if c.curSpans == 0 {
		return nil
	}
	wire, err := proto.Marshal(c.cur)
	if err != nil {
		return fmt.Errorf("marshal OTLP traces: %w", err)
	}
	if len(wire) > c.maxBytes {
		return &PermanentError{Reason: fmt.Sprintf("marshaled trace batch (%d bytes) exceeds %d-byte limit", len(wire), c.maxBytes)}
	}
	ch := Chunk{Path: otlpTracesPath, Body: wire, Records: c.curSpans}
	c.cur = &tracepb.TracesData{}
	c.curSpans = 0
	c.estBytes = 0
	return c.onChunk(ch)
}
