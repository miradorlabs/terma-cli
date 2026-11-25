package codex

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const (
	codexOTLPScope      = "codex_otel.log_only"
	codexTermaScope     = "terma-cli"
	codexDefaultService = "codex_cli_rs"
	codexDefaultOrigin  = "codex_cli_rs"
	codexDefaultProv    = "OpenAI"
	codexEventTimeFmt   = "2006-01-02T15:04:05.000Z"
	codexMaxToolOutput  = 16 << 10
)

func (p *codexHistoryParser) setCwd(cwd string) error {
	if cwd != p.cwd {
		for callID := range p.pendingCalls {
			p.priorCwdCalls[callID] = true
		}
		clear(p.pendingCalls)
		if !p.segStarted {
			p.segReaches = nil
		}
	}
	if p.segStarted && cwd != p.segCwd {
		if err := p.flushSegment(); err != nil {
			return err
		}
	}
	p.cwd = cwd
	return nil
}

func (p *codexHistoryParser) ensureSegment(ts time.Time) {
	if !p.segStarted {
		p.segStarted = true
		p.segCwd = p.cwd
		p.segHasStart = false
	}
	if !p.segHasStart {
		p.segHasStart = true
		p.segNative = append(p.segNative, p.makeConversationStartRecord(ts))
	}
}

func (p *codexHistoryParser) uniqueNanos(kind string, ts time.Time) uint64 {
	var nanos uint64
	if !ts.IsZero() && ts.UnixNano() > 0 {
		nanos = uint64(ts.UnixNano())
	}
	if prev := p.lastEventNanos[kind]; nanos <= prev {
		nanos = prev + 1
	}
	p.lastEventNanos[kind] = nanos
	return nanos
}

func (p *codexHistoryParser) makeConversationStartRecord(eventTS time.Time) *logspb.LogRecord {
	// Stamp conversation_starts with the triggering event's timestamp (falling back to
	// session_meta) so resumed segments and Mixed-run cutoffs evaluate in the segment's
	// own run rather than borrowing an earlier checkout's start time.
	ts := eventTS
	if ts.IsZero() {
		ts = p.metaTime
	}
	nanos := uint64(0)
	if !ts.IsZero() && ts.UnixNano() > 0 {
		nanos = uint64(ts.UnixNano())
	}
	attrs := []*commonpb.KeyValue{
		cStrKV("event.name", "codex.conversation_starts"),
		cStrKV("conversation.id", p.sessionID),
		cStrKV("provider_name", p.modelProvider),
		cStrKV("originator", p.originator),
		cBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if p.cliVersion != "" {
		attrs = append(attrs, cStrKV("app.version", p.cliVersion))
	}
	if m := cmp.Or(p.model, p.firstModel); m != "" {
		attrs = append(attrs, cStrKV("model", m))
	}
	if e := cmp.Or(p.effort, p.firstEffort); e != "" {
		attrs = append(attrs, cStrKV("reasoning_effort", e))
	}
	if !ts.IsZero() {
		attrs = append(attrs, cStrKV("event.timestamp", ts.UTC().Format(codexEventTimeFmt)))
	}
	return &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.conversation_starts"}},
		Attributes:           attrs,
	}
}

func (p *codexHistoryParser) emitUserPrompt(text string, ts time.Time) error {
	if p.lastPromptText == text && p.lastPromptTurn == p.turnID {
		return nil
	}
	p.lastPromptText = text
	p.lastPromptTurn = p.turnID
	p.ensureSegment(ts)
	nanos := p.uniqueNanos("codex.user_prompt", ts)
	attrs := p.commonNativeAttrs("codex.user_prompt", ts)
	attrs = append(attrs,
		cStrKV("prompt", text),
		cIntKV("prompt_length", int64(len(text))),
	)
	p.segNative = append(p.segNative, &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		TraceId:              p.traceBytes,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.user_prompt"}},
		Attributes:           attrs,
	})
	return nil
}

func (p *codexHistoryParser) emitModelCall(u codexUsageNumbers, ts time.Time) error {
	p.lastPromptText = ""
	p.ensureSegment(ts)
	nanos := p.uniqueNanos("codex.sse_event", ts)
	attrs := p.commonNativeAttrs("codex.sse_event", ts)
	attrs = append(attrs,
		cStrKV("event.kind", "response.completed"),
		cIntKV("input_token_count", u.InputTokens),
		cIntKV("output_token_count", u.OutputTokens),
		cIntKV("cached_token_count", u.CachedInputTokens),
		cIntKV("reasoning_token_count", u.ReasoningOutputTokens),
		cIntKV("tool_token_count", u.TotalTokens),
	)
	if u.CacheWriteInputTokens > 0 {
		attrs = append(attrs, cIntKV("cache_write_token_count", u.CacheWriteInputTokens))
	}
	if p.effort != "" {
		attrs = append(attrs, cStrKV("model_reasoning_effort", p.effort))
	}
	p.segNative = append(p.segNative, &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		TraceId:              p.traceBytes,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.sse_event"}},
		Attributes:           attrs,
	})
	return nil
}

func (p *codexHistoryParser) emitToolResult(callID, toolName, arguments, output string, success bool, durationMs int64, trace []byte, ts time.Time) error {
	p.ensureSegment(ts)
	nanos := uint64(0)
	if !ts.IsZero() && ts.UnixNano() > 0 {
		nanos = uint64(ts.UnixNano())
	}
	outText, _ := cutUTF8(output, codexMaxToolOutput)
	attrs := p.commonNativeAttrs("codex.tool_result", ts)
	attrs = append(attrs,
		cStrKV("call_id", callID),
		cBoolKV("success", success),
	)
	if toolName != "" {
		attrs = append(attrs, cStrKV("tool_name", toolName))
	}
	if arguments != "" {
		attrs = append(attrs, cStrKV("arguments", arguments))
	}
	if outText != "" {
		attrs = append(attrs, cStrKV("output", outText))
	}
	if durationMs >= 0 {
		attrs = append(attrs, cIntKV("duration_ms", durationMs))
	}
	p.segNative = append(p.segNative, &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		TraceId:              trace,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
		Attributes:           attrs,
	})
	return nil
}

func (p *codexHistoryParser) emitAssistantReply(msgID, text, phase string, ts time.Time) error {
	p.lastPromptText = ""
	p.ensureSegment(ts)
	cutText, truncated := cutUTF8(text, codexReplyMaxText)
	nanos := uint64(0)
	if !ts.IsZero() && ts.UnixNano() > 0 {
		nanos = uint64(ts.UnixNano())
	}
	attrs := []*commonpb.KeyValue{
		cStrKV(semconv.SessionIDKey, p.sessionID),
		cStrKV(semconv.GenAIMainAgentNameKey, codexTool),
		cStrKV(semconv.TermaEvidenceSourceKey, sourceCodexRollout),
		cStrKV(semconv.TermaMessageIDKey, msgID),
		cStrKV(semconv.TermaMessageTextKey, cutText),
		cBoolKV(semconv.TermaMessageTruncatedKey, truncated),
		cBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if p.turnID != "" {
		attrs = append(attrs, cStrKV(semconv.TermaTurnIDKey, p.turnID))
	}
	if phase != "" {
		attrs = append(attrs, cStrKV(semconv.TermaMessagePhaseKey, phase))
	}
	if p.model != "" {
		attrs = append(attrs, cStrKV(semconv.GenAIRequestModelKey, p.model))
	}
	p.segSupplements = append(p.segSupplements, &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		TraceId:              p.traceBytes,
		EventName:            semconv.TermaAssistantMessageEvent,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: semconv.TermaAssistantMessageEvent}},
		Attributes:           attrs,
	})
	return nil
}

func (p *codexHistoryParser) commonNativeAttrs(eventName string, ts time.Time) []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{
		cStrKV("event.name", eventName),
		cStrKV("conversation.id", p.sessionID),
		cStrKV("originator", p.originator),
		cBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if p.cliVersion != "" {
		attrs = append(attrs, cStrKV("app.version", p.cliVersion))
	}
	if p.model != "" {
		attrs = append(attrs, cStrKV("model", p.model))
	}
	if !ts.IsZero() {
		attrs = append(attrs, cStrKV("event.timestamp", ts.UTC().Format(codexEventTimeFmt)))
	}
	return attrs
}

func (p *codexHistoryParser) flushSegment() error {
	if len(p.segNative) == 0 && len(p.segSupplements) == 0 && len(p.segReaches) == 0 {
		p.segStarted = false
		p.segReaches = nil
		return nil
	}
	nativeRecs := p.segNative
	suppRecs := p.segSupplements
	reaches := p.segReaches
	cwd := p.segCwd
	p.segNative, p.segSupplements, p.segReaches = nil, nil, nil
	p.segStarted, p.segHasStart = false, false

	seg := agents.HistorySegment{
		SourceSystem: codexSourceSystem,
		SessionID:    p.sessionID,
		Cwd:          cwd,
		ToolReaches:  reaches,
		RecordCount:  len(nativeRecs) + len(suppRecs),
	}
	if cwd != "" && cwd == p.metaCwd {
		seg.FallbackRepoURL = p.metaRepoURL
		seg.FallbackBranch = p.metaBranch
	}
	if len(nativeRecs) > 0 {
		resAttrs := []*commonpb.KeyValue{
			cStrKV("service.name", cmp.Or(p.originator, codexDefaultService)),
			cBoolKV(semconv.TermaSessionHistoricalKey, true),
		}
		if p.cliVersion != "" {
			resAttrs = append(resAttrs, cStrKV("service.version", p.cliVersion))
		}
		seg.NativeLogs = &logspb.LogsData{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: resAttrs},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      &commonpb.InstrumentationScope{Name: codexOTLPScope, Version: p.cliVersion},
					LogRecords: nativeRecs,
				}},
			}},
		}
	}
	if len(suppRecs) > 0 {
		seg.SupplementalLogs = &logspb.LogsData{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					cStrKV(semconv.ServiceNameKey, codexTermaScope),
					cStrKV(semconv.TermaSchemaVersionKey, semconv.SchemaVersion),
					cBoolKV(semconv.TermaSessionHistoricalKey, true),
				}},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      &commonpb.InstrumentationScope{Name: codexTermaScope},
					LogRecords: suppRecs,
				}},
			}},
		}
	}
	return p.emit(seg)
}

func kindsOf(m *struct {
	ContentItemKinds []string `json:"content_item_kinds"`
}) []string {
	if m == nil {
		return nil
	}
	return m.ContentItemKinds
}

func isCodexUserPrompt(text string, hasMeta bool, kinds []string) bool {
	if hasMeta && len(kinds) > 0 {
		return slices.Contains(kinds, "user.text")
	}
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<environment_context>", "<permissions", "<INSTRUCTIONS>",
		"<AGENTS.md>", "<collaboration_mode>", "<skills_instructions>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

func joinCodexText(parts []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}, wantType string) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == wantType || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func decodeCodexOutput(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	if trimmed[0] == '[' {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(trimmed, &parts) == nil {
			var b strings.Builder
			for _, part := range parts {
				b.WriteString(part.Text)
			}
			return b.String()
		}
	}
	return string(trimmed)
}

func cutUTF8(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func parseCodexTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func cStrKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func cIntKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func cBoolKV(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}
