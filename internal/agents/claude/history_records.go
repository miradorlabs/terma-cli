package claude

import (
	"bytes"
	"cmp"
	"encoding/json"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const (
	claudeOTLPScope      = "com.anthropic.claude_code.events"
	claudeBodyPrefix     = "claude_code."
	claudeEventTimeFmt   = "2006-01-02T15:04:05.000Z"
	claudeServiceCLI     = "claude-code"
	claudeServiceDesktop = "claude-code-desktop"
	claudeMaxSegRecords  = 256
)

func (p *claudeHistoryParser) flushPendingCall() error {
	c := p.pending
	if c == nil {
		return nil
	}
	p.pending = nil
	if c.hasUsage || c.isAPIError {
		if err := p.appendToSegment(c.cwd, c.branch, p.makeAPIRequestRecord(c)); err != nil {
			return err
		}
	}
	if !c.isAPIError && len(c.textParts) > 0 {
		text := strings.Join(c.textParts, "\n")
		ts := c.textAt
		if ts.IsZero() {
			ts = c.at
		}
		if err := p.appendToSegment(c.cwd, c.branch, p.makeAssistantResponseRecord(c, text, ts)); err != nil {
			return err
		}
	}
	return nil
}

func (p *claudeHistoryParser) appendToSegment(cwd, branch string, lr *logspb.LogRecord) error {
	if p.segStarted && (cwd != p.segCwd || (branch != "" && p.segBranch != "" && branch != p.segBranch)) {
		if err := p.flushSegment(); err != nil {
			return err
		}
	}
	p.segStarted = true
	p.segCwd = cwd
	p.segBranch = cmp.Or(p.segBranch, branch)
	p.segRecords = append(p.segRecords, lr)
	if len(p.segRecords) >= claudeMaxSegRecords {
		return p.flushSegment()
	}
	return nil
}

func (p *claudeHistoryParser) flushSegment() error {
	if len(p.segRecords) == 0 {
		p.segStarted = false
		p.segBranch = ""
		return nil
	}
	recs := p.segRecords
	cwd := p.segCwd
	branch := p.segBranch
	p.segRecords = nil
	p.segStarted = false
	p.segBranch = ""
	var logs *logspb.LogsData
	if len(recs) > 0 {
		resAttrs := []*commonpb.KeyValue{
			strKV("service.name", p.serviceName),
			boolKV(semconv.TermaSessionHistoricalKey, true),
		}
		if p.serviceVersion != "" {
			resAttrs = append(resAttrs, strKV("service.version", p.serviceVersion))
		}
		logs = &logspb.LogsData{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: resAttrs},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      &commonpb.InstrumentationScope{Name: claudeOTLPScope, Version: p.serviceVersion},
					LogRecords: recs,
				}},
			}},
		}
	}
	return p.emit(agents.HistorySegment{
		SourceSystem:   claudeSourceSystem,
		SessionID:      p.sessionID,
		Cwd:            cwd,
		FallbackBranch: branch,
		NativeLogs:     logs,
		RecordCount:    len(recs),
	})
}

func (p *claudeHistoryParser) nextSeq() int64 {
	p.seq++
	return p.seq
}

func (p *claudeHistoryParser) baseRecord(eventName, sessionID, agentID, promptID string, ts time.Time) (*logspb.LogRecord, []*commonpb.KeyValue) {
	nanos := uint64(0)
	if !ts.IsZero() && ts.UnixNano() > 0 {
		nanos = uint64(ts.UnixNano())
	}
	attrs := []*commonpb.KeyValue{
		strKV("event.name", eventName),
		strKV("session.id", sessionID),
		intKV("event.sequence", p.nextSeq()),
		boolKV(semconv.TermaSessionHistoricalKey, true),
	}
	if !ts.IsZero() {
		attrs = append(attrs, strKV("event.timestamp", ts.UTC().Format(claudeEventTimeFmt)))
	}
	if promptID != "" {
		attrs = append(attrs, strKV("prompt.id", promptID))
	}
	if agentID != "" {
		attrs = append(attrs, strKV("agent_id", agentID))
	}
	lr := &logspb.LogRecord{
		TimeUnixNano:         nanos,
		ObservedTimeUnixNano: nanos,
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: claudeBodyPrefix + eventName}},
	}
	return lr, attrs
}

func (p *claudeHistoryParser) makeUserPromptRecord(sessionID, agentID, msgUUID, promptID, text string, ts time.Time) *logspb.LogRecord {
	lr, attrs := p.baseRecord("user_prompt", sessionID, agentID, promptID, ts)
	if msgUUID != "" {
		attrs = append(attrs, strKV("message.uuid", msgUUID))
	}
	attrs = append(attrs,
		strKV("prompt", text),
		intKV("prompt_length", int64(len(text))),
	)
	lr.Attributes = attrs
	return lr
}

func (p *claudeHistoryParser) makeAPIRequestRecord(c *claudePendingCall) *logspb.LogRecord {
	eventName := "api_request"
	if c.isAPIError {
		eventName = "api_error"
	}
	lr, attrs := p.baseRecord(eventName, c.sessionID, c.agentID, c.promptID, c.at)
	if c.requestID != "" {
		attrs = append(attrs, strKV("request_id", c.requestID))
	} else if c.requestRef == "" {
		attrs = append(attrs, strKV("request_id", cmp.Or(c.messageID, c.key)))
	}
	if c.requestRef != "" {
		attrs = append(attrs, strKV("client_request_id", c.requestRef))
	}
	if c.model != "" {
		attrs = append(attrs, strKV("model", c.model))
	}
	if c.effort != "" {
		attrs = append(attrs, strKV("effort", c.effort))
	}
	if c.querySource != "" {
		attrs = append(attrs, strKV("query_source", c.querySource))
	}
	if c.speed != "" {
		attrs = append(attrs, strKV("speed", c.speed))
	}
	if c.stopReason != "" {
		attrs = append(attrs, strKV("stop_reason", c.stopReason))
	}
	if c.hasUsage {
		attrs = append(attrs,
			intKV("input_tokens", c.inputTokens),
			intKV("output_tokens", c.outputTokens),
			intKV("cache_read_tokens", c.cacheRead),
			intKV("cache_creation_tokens", c.cacheCreation),
		)
		if c.cache5m > 0 {
			attrs = append(attrs, intKV("cache_creation_5m_tokens", c.cache5m))
		}
		if c.cache1h > 0 {
			attrs = append(attrs, intKV("cache_creation_1h_tokens", c.cache1h))
		}
	}
	if c.costUSD != nil && *c.costUSD >= 0 {
		attrs = append(attrs, doubleKV("cost_usd", *c.costUSD))
	}
	if c.durationMs != nil && *c.durationMs >= 0 {
		attrs = append(attrs, intKV("duration_ms", *c.durationMs))
	}
	lr.Attributes = attrs
	return lr
}

func (p *claudeHistoryParser) makeAssistantResponseRecord(c *claudePendingCall, text string, ts time.Time) *logspb.LogRecord {
	lr, attrs := p.baseRecord("assistant_response", c.sessionID, c.agentID, c.promptID, ts)
	if c.textUUID != "" {
		attrs = append(attrs, strKV("message.uuid", c.textUUID))
	}
	if c.requestID != "" {
		attrs = append(attrs, strKV("request_id", c.requestID))
	} else if c.requestRef != "" {
		attrs = append(attrs, strKV("client_request_id", c.requestRef))
	}
	if c.model != "" {
		attrs = append(attrs, strKV("model", c.model))
	}
	attrs = append(attrs,
		strKV("response", text),
		intKV("response_length", int64(len(text))),
	)
	lr.Attributes = attrs
	return lr
}

func (p *claudeHistoryParser) makeToolResultRecord(sessionID, agentID, promptID, toolUseID string, tool claudePendingTool, blk claudeContentBlock, ts time.Time) *logspb.LogRecord {
	lr, attrs := p.baseRecord("tool_result", sessionID, agentID, promptID, ts)
	attrs = append(attrs,
		strKV("tool_use_id", toolUseID),
		boolKV("success", !blk.IsError),
	)
	if tool.Name != "" {
		attrs = append(attrs, strKV("tool_name", tool.Name))
	}
	if tool.InputJSON != "" {
		attrs = append(attrs,
			strKV("tool_input", tool.InputJSON),
			intKV("tool_input_size_bytes", int64(len(tool.InputJSON))),
		)
	}
	if n := len(bytes.TrimSpace(blk.Content)); n > 0 {
		attrs = append(attrs, intKV("tool_result_size_bytes", int64(n)))
	}
	if !tool.At.IsZero() && !ts.IsZero() && !ts.Before(tool.At) {
		attrs = append(attrs, intKV("duration_ms", ts.Sub(tool.At).Milliseconds()))
	}
	lr.Attributes = attrs
	return lr
}

func (p *claudeHistoryParser) makeToolDecisionRecord(sessionID, agentID, promptID, toolUseID, toolName, decision, source string, ts time.Time) *logspb.LogRecord {
	lr, attrs := p.baseRecord("tool_decision", sessionID, agentID, promptID, ts)
	attrs = append(attrs,
		strKV("tool_use_id", toolUseID),
		strKV("decision", decision),
	)
	if toolName != "" {
		attrs = append(attrs, strKV("tool_name", toolName))
	}
	if source != "" {
		attrs = append(attrs, strKV("source", source))
	}
	lr.Attributes = attrs
	return lr
}

func parseClaudeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func compactRawJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, trimmed) == nil {
		return buf.String()
	}
	return string(trimmed)
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func doubleKV(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

func boolKV(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}
