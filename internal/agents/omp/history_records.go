package omp

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const (
	ompMaxSegRecords = 256
	ompMaxToolText   = 64 << 10
	ompMaxToolArgs   = 64 << 10
	ompMaxRawDetails = 64 << 10
)

type ompOtelPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	Modality  string          `json:"modality,omitempty"`
	MimeType  string          `json:"mime_type,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ompOtelMessage struct {
	Role         string        `json:"role"`
	Parts        []ompOtelPart `json:"parts"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

func (p *ompHistoryParser) ensureSubagentStart(ts time.Time) {
	if !p.includeSupplemental || !p.subagent || p.subagentStartSent {
		return
	}
	if p.parentSessionID == "" || p.curSessionID == "" || p.parentSessionID == p.curSessionID {
		return
	}
	p.subagentStartSent = true
	at := p.headerTime
	if at.IsZero() {
		at = ts
	}
	p.segSupp = append(p.segSupp, p.makeSubagentStartRecord(at))
}

func (p *ompHistoryParser) emitTitle(title string, ts time.Time) error {
	return p.emitTitleRecord(title, ts, ts)
}

func (p *ompHistoryParser) emitUndatedTitle(title string, observed time.Time) error {
	return p.emitTitleRecord(title, time.Time{}, observed)
}

func (p *ompHistoryParser) emitTitleRecord(title string, ts, observed time.Time) error {
	if !p.includeSupplemental || p.curSessionID == "" {
		return nil
	}
	title = hookrun.TruncateBytes(strings.TrimSpace(title), hookrun.TitleMaxBytes)
	if title == "" || title == p.lastEmittedTitle {
		return nil
	}
	p.lastEmittedTitle = title
	if !ts.IsZero() {
		p.lastTitleAt = ts
	}
	attrs := []*commonpb.KeyValue{
		oStrKV(semconv.SessionIDKey, p.curSessionID),
		oStrKV(semconv.GenAIMainAgentNameKey, name),
		oStrKV(semconv.TermaEvidenceSourceKey, sourceOmpSession),
		oStrKV(semconv.TermaSessionTitleKey, title),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	p.segSupp = append(p.segSupp, &logspb.LogRecord{
		TimeUnixNano:         timeNanos(ts),
		ObservedTimeUnixNano: timeNanos(observed),
		EventName:            semconv.TermaSessionTitleEvent,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: semconv.TermaSessionTitleEvent}},
		Attributes:           attrs,
	})
	return p.maybeFlushSegment()
}

func (p *ompHistoryParser) makeSubagentStartRecord(ts time.Time) *logspb.LogRecord {
	nanos := timeNanos(ts)
	attrs := []*commonpb.KeyValue{
		oStrKV(semconv.SessionIDKey, p.curSessionID),
		oStrKV(semconv.GenAIMainAgentNameKey, name),
		oStrKV(semconv.TermaSessionParentIDKey, p.parentSessionID),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if p.agentName != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIAgentNameKey, p.agentName))
	}
	if p.configuredModel != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIRequestModelKey, p.configuredModel))
	}
	return &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		EventName:            semconv.TermaSessionStartEvent,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: semconv.TermaSessionStartEvent}},
		Attributes:           attrs,
	}
}

func (p *ompHistoryParser) makeFilesTouchedRecord(toolName, callID string, files []string, ts time.Time) *logspb.LogRecord {
	nanos := timeNanos(ts)
	attrs := []*commonpb.KeyValue{
		oStrKV(semconv.SessionIDKey, p.curSessionID),
		oStrKV(semconv.GenAIMainAgentNameKey, name),
		oStrKV(semconv.GenAIToolNameKey, toolName),
		oStrKV(semconv.GenAIToolCallIDKey, callID),
		oStrArrayKV(semconv.TermaFilesPathsKey, files),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	return &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		EventName:            semconv.TermaFilesTouchedEvent,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: semconv.TermaFilesTouchedEvent}},
		Attributes:           attrs,
	}
}

func (p *ompHistoryParser) makeChatSpan(entryID string, msg *ompMessage, blocks []ompContentBlock, step int, start, end time.Time) *tracepb.Span {
	reqModel := cmp.Or(p.configuredModel, msg.Model)
	promoted := p.configuredModel != "" && msg.Model != "" && msg.Model != p.configuredModel
	respModel := cmp.Or(msg.UpstreamModel, msg.Model, reqModel)
	if promoted {
		respModel = msg.Model
	}
	provider := mapOmpProviderToOtel(cmp.Or(msg.Provider, p.configuredProvider))
	spanName := "chat"
	if reqModel != "" {
		spanName = "chat " + reqModel
	}
	attrs := []*commonpb.KeyValue{
		oStrKV("gen_ai.operation.name", "chat"),
		oStrKV("gen_ai.conversation.id", p.curSessionID),
		oIntKV("omp.gen_ai.agent.step.number", int64(step)),
		oStrKV("gen_ai.output.type", "text"),
		oBoolKV("gen_ai.request.stream", true),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if reqModel != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIRequestModelKey, reqModel))
	}
	if respModel != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIResponseModelKey, respModel))
	}
	if provider != "" {
		attrs = append(attrs, oStrKV("gen_ai.provider.name", provider))
	}
	if msg.ResponseID != "" {
		attrs = append(attrs, oStrKV("gen_ai.response.id", msg.ResponseID))
	}
	if p.reasoningEffort != "" {
		attrs = append(attrs, oStrKV("omp.gen_ai.request.reasoning.effort", p.reasoningEffort))
	}
	if msg.TTFT != nil && *msg.TTFT >= 0 {
		attrs = append(attrs, oDoubleKV("gen_ai.response.time_to_first_chunk", float64(*msg.TTFT)/1000.0))
	}
	if fr := mapOmpStopReason(msg.StopReason); fr != "" {
		attrs = append(attrs, oStrArrayKV(semconv.GenAIResponseFinishReasonsKey, []string{fr}))
	}
	attrs = appendAgentIdentityAttrs(attrs, p.subagent, p.agentID, p.agentName)
	if len(p.availableTools) > 0 && step == 0 {
		attrs = append(attrs, oStrArrayKV("omp.gen_ai.request.available_tools", p.availableTools))
	}
	attrs = appendUsageAttrs(attrs, msg.Usage)

	if len(p.systemPrompt) > 0 && !p.systemPromptSent {
		p.systemPromptSent = true
		sysParts := make([]ompOtelPart, 0, len(p.systemPrompt))
		for _, text := range p.systemPrompt {
			if text != "" {
				sysParts = append(sysParts, ompOtelPart{Type: "text", Content: text})
			}
		}
		if raw := marshalCompactJSON(sysParts); raw != "" {
			attrs = append(attrs, oStrKV("gen_ai.system_instructions", raw))
		}
	}
	if p.pendingPrompt != nil && !p.pendingPrompt.Attached {
		p.pendingPrompt.Attached = true
		if raw := marshalCompactJSON([]ompOtelMessage{{Role: "user", Parts: p.pendingPrompt.Parts}}); raw != "" {
			attrs = append(attrs, oStrKV("gen_ai.input.messages", raw))
		}
	}
	if outParts := assistantBlocksToOtelParts(blocks); len(outParts) > 0 {
		fr := cmp.Or(mapOmpStopReason(msg.StopReason), msg.StopReason, "stop")
		if raw := marshalCompactJSON([]ompOtelMessage{{Role: "assistant", Parts: outParts, FinishReason: fr}}); raw != "" {
			attrs = append(attrs, oStrKV("gen_ai.output.messages", raw))
		}
	}

	var status *tracepb.Status
	if msg.StopReason == "error" || msg.StopReason == "aborted" {
		attrs = append(attrs, oStrKV(semconv.ErrorTypeKey, msg.StopReason))
		status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	}

	spanKey := "chat:" + cmp.Or(entryID, fmt.Sprintf("%d", p.chatSeq))
	return &tracepb.Span{
		TraceId:           p.turnTraceID,
		SpanId:            deriveSpanID(p.curSessionID, spanKey),
		ParentSpanId:      p.turnInvokeSpanID,
		Name:              spanName,
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: timeNanos(start),
		EndTimeUnixNano:   timeNanos(end),
		Attributes:        attrs,
		Status:            status,
	}
}

func (p *ompHistoryParser) makeExecuteToolSpan(entryID string, msg *ompMessage, tool ompPendingTool, toolName string, details *ompToolDetails, start, end time.Time) *tracepb.Span {
	failed := msg.IsError || (details != nil && details.ExitCode != nil && *details.ExitCode != 0)
	toolStatus := "ok"
	var status *tracepb.Status
	if failed {
		toolStatus = "error"
		status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	}
	attrs := []*commonpb.KeyValue{
		oStrKV("gen_ai.operation.name", "execute_tool"),
		oStrKV("gen_ai.conversation.id", p.curSessionID),
		oStrKV(semconv.GenAIToolNameKey, toolName),
		oStrKV(semconv.GenAIToolCallIDKey, msg.ToolCallID),
		oStrKV(semconv.GenAIToolTypeKey, "function"),
		oStrKV("omp.gen_ai.tool.status", toolStatus),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	attrs = appendAgentIdentityAttrs(attrs, p.subagent, p.agentID, p.agentName)
	if tool.ArgsJSON != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIToolCallArgumentsKey, tool.ArgsJSON))
	}
	if resultJSON := buildOmpToolResultJSON(msg.Content, msg.Details, details, msg.IsError); resultJSON != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIToolCallResultKey, resultJSON))
	}

	spanKey := "tool:" + cmp.Or(msg.ToolCallID, entryID, fmt.Sprintf("%d", p.toolSeq))
	return &tracepb.Span{
		TraceId:           tool.TraceID,
		SpanId:            deriveSpanID(p.curSessionID, spanKey),
		ParentSpanId:      tool.ParentSpanID,
		Name:              strings.TrimSpace("execute_tool " + toolName),
		Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: timeNanos(start),
		EndTimeUnixNano:   timeNanos(end),
		Attributes:        attrs,
		Status:            status,
	}
}

func (p *ompHistoryParser) makeAuxiliarySpan(rec *ompLine, start, end time.Time) *tracepb.Span {
	isJudgment := len(bytes.TrimSpace(rec.Judgment)) > 0 && !bytes.Equal(bytes.TrimSpace(rec.Judgment), []byte("null"))
	op := "chat"
	if isJudgment {
		op = "judgment"
	}
	spanName := strings.TrimSpace(op + " " + rec.Model)
	attrs := []*commonpb.KeyValue{
		oStrKV("gen_ai.operation.name", op),
		oStrKV("gen_ai.conversation.id", p.curSessionID),
		oBoolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if isJudgment {
		if rec.Purpose != "" {
			attrs = append(attrs, oStrKV("omp.gen_ai.judgment.purpose", rec.Purpose))
		}
	} else {
		attrs = append(attrs, oIntKV("omp.gen_ai.agent.step.number", -1))
		if rec.Purpose != "" {
			attrs = append(attrs, oStrKV("omp.gen_ai.oneshot.kind", rec.Purpose))
		}
	}
	if rec.Model != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIRequestModelKey, rec.Model), oStrKV(semconv.GenAIResponseModelKey, rec.Model))
	}
	if prov := mapOmpProviderToOtel(rec.Provider); prov != "" {
		attrs = append(attrs, oStrKV("gen_ai.provider.name", prov))
	}
	if rec.TTFT != nil && *rec.TTFT >= 0 {
		attrs = append(attrs, oDoubleKV("gen_ai.response.time_to_first_chunk", float64(*rec.TTFT)/1000.0))
	}
	if fr := mapOmpStopReason(rec.StopReason); fr != "" {
		attrs = append(attrs, oStrArrayKV(semconv.GenAIResponseFinishReasonsKey, []string{fr}))
	}
	attrs = appendAgentIdentityAttrs(attrs, p.subagent, p.agentID, p.agentName)
	attrs = appendUsageAttrs(attrs, rec.Usage)
	var status *tracepb.Status
	if rec.StopReason == "error" || rec.StopReason == "aborted" {
		attrs = append(attrs, oStrKV(semconv.ErrorTypeKey, rec.StopReason))
		status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	}
	spanKey := "usage:" + cmp.Or(rec.ID, fmt.Sprintf("%d", p.usageSeq))
	return &tracepb.Span{
		TraceId:           deriveTraceID(p.curSessionID, spanKey),
		SpanId:            deriveSpanID(p.curSessionID, spanKey),
		Name:              spanName,
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: timeNanos(start),
		EndTimeUnixNano:   timeNanos(end),
		Attributes:        attrs,
		Status:            status,
	}
}

func (p *ompHistoryParser) maybeFlushSegment() error {
	if len(p.segSpans)+len(p.segSupp) >= ompMaxSegRecords {
		return p.flushSegment()
	}
	return nil
}

func (p *ompHistoryParser) flushSegment() error {
	if len(p.segSpans) == 0 && len(p.segSupp) == 0 && len(p.segReaches) == 0 {
		return nil
	}
	spans := p.segSpans
	supp := p.segSupp
	reaches := p.segReaches
	p.segSpans, p.segSupp, p.segReaches = nil, nil, nil

	seg := agents.HistorySegment{
		SourceSystem: ompSourceSystem,
		SessionID:    p.rootSessionID,
		Cwd:          p.cwd,
		ToolReaches:  reaches,
		Subagent:     p.subagent,
		RecordCount:  len(spans) + len(supp),
	}
	if len(spans) > 0 {
		resAttrs := []*commonpb.KeyValue{
			oStrKV(semconv.ServiceNameKey, ompServiceName),
			oBoolKV(semconv.TermaSessionHistoricalKey, true),
		}
		if p.serviceVersion != "" {
			resAttrs = append(resAttrs, oStrKV(semconv.ServiceVersionKey, p.serviceVersion))
		}
		if p.subagent && p.parentSessionID != "" && p.parentSessionID != p.curSessionID {
			resAttrs = append(resAttrs, oStrKV(semconv.TermaSessionParentIDKey, p.parentSessionID))
		}
		seg.NativeTraces = &tracepb.TracesData{
			ResourceSpans: []*tracepb.ResourceSpans{{
				Resource: &resourcepb.Resource{Attributes: resAttrs},
				ScopeSpans: []*tracepb.ScopeSpans{{
					Scope: &commonpb.InstrumentationScope{Name: traceScope},
					Spans: spans,
				}},
			}},
		}
	}
	if len(supp) > 0 {
		seg.SupplementalLogs = &logspb.LogsData{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					oStrKV(semconv.ServiceNameKey, ompTermaScope),
					oStrKV(semconv.TermaSchemaVersionKey, semconv.SchemaVersion),
					oBoolKV(semconv.TermaSessionHistoricalKey, true),
				}},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      &commonpb.InstrumentationScope{Name: ompTermaScope},
					LogRecords: supp,
				}},
			}},
		}
	}
	return p.emit(seg)
}

func oStrKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func oIntKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func oDoubleKV(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

func oBoolKV(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}

func oStrArrayKV(k string, vals []string) *commonpb.KeyValue {
	out := make([]*commonpb.AnyValue, len(vals))
	for i, v := range vals {
		out[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}
	}
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: out}}}}
}
