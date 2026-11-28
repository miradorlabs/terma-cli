package omp

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

type ompLine struct {
	Type                 string          `json:"type"`
	ID                   string          `json:"id"`
	Timestamp            string          `json:"timestamp"`
	UpdatedAt            string          `json:"updatedAt"`
	Title                string          `json:"title"`
	Cwd                  string          `json:"cwd"`
	ParentSession        string          `json:"parentSession"`
	ContinuationOf       string          `json:"continuationOf"`
	PreviousSessionFiles []string        `json:"previousSessionFiles"`
	ThinkingLevel        string          `json:"thinkingLevel"`
	Configured           string          `json:"configured"`
	Model                string          `json:"model"`
	Role                 string          `json:"role"`
	Purpose              string          `json:"purpose"`
	Provider             string          `json:"provider"`
	StopReason           string          `json:"stopReason"`
	Duration             *float64        `json:"duration"`
	TTFT                 *float64        `json:"ttft"`
	Usage                *ompUsage       `json:"usage"`
	Judgment             json.RawMessage `json:"judgment"`
	Agent                string          `json:"agent"`
	Tools                []string        `json:"tools"`
	SystemPrompt         json.RawMessage `json:"systemPrompt"`
	Message              json.RawMessage `json:"message"`
	Runtime              *struct {
		Version string `json:"version"`
	} `json:"runtime"`
}

type ompUsage struct {
	Input           int64  `json:"input"`
	Output          int64  `json:"output"`
	CacheRead       int64  `json:"cacheRead"`
	CacheWrite      int64  `json:"cacheWrite"`
	TotalTokens     int64  `json:"totalTokens"`
	ReasoningTokens *int64 `json:"reasoningTokens"`
	Cost            *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	} `json:"cost"`
}

type ompMessage struct {
	Role          string          `json:"role"`
	Content       json.RawMessage `json:"content"`
	Synthetic     bool            `json:"synthetic"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model"`
	UpstreamModel string          `json:"upstreamModel"`
	ResponseID    string          `json:"responseId"`
	StopReason    string          `json:"stopReason"`
	Usage         *ompUsage       `json:"usage"`
	Timestamp     int64           `json:"timestamp"`
	Duration      *float64        `json:"duration"`
	TTFT          *float64        `json:"ttft"`
	CompletedAt   int64           `json:"completedAt"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Details       json.RawMessage `json:"details"`
	IsError       bool            `json:"isError"`
}

type ompContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Data      string          `json:"data"`
	MimeType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func appendAgentIdentityAttrs(attrs []*commonpb.KeyValue, subagent bool, agentID, agentName string) []*commonpb.KeyValue {
	if !subagent {
		return attrs
	}
	if agentID != "" {
		attrs = append(attrs, oStrKV("gen_ai.agent.id", agentID))
	}
	if agentName != "" {
		attrs = append(attrs, oStrKV(semconv.GenAIAgentNameKey, agentName))
	}
	return attrs
}

func appendUsageAttrs(attrs []*commonpb.KeyValue, u *ompUsage) []*commonpb.KeyValue {
	if u == nil {
		return attrs
	}
	// OMP's live exporter sums non-cached input + cacheRead + cacheWrite into
	// gen_ai.usage.input_tokens (telemetry.ts:applyUsageAttributes).
	inputTotal := u.Input + u.CacheRead + u.CacheWrite
	total := u.TotalTokens
	if total == 0 {
		total = inputTotal + u.Output
	}
	attrs = append(attrs,
		oIntKV(semconv.GenAIUsageInputTokensKey, inputTotal),
		oIntKV(semconv.GenAIUsageOutputTokensKey, u.Output),
		oIntKV("omp.gen_ai.usage.total_tokens", total),
		oIntKV(semconv.GenAIUsageCacheReadInputTokensKey, u.CacheRead),
		oIntKV("gen_ai.usage.cache_creation.input_tokens", u.CacheWrite),
	)
	if u.ReasoningTokens != nil {
		attrs = append(attrs, oIntKV(semconv.GenAIUsageReasoningOutputTokensKey, *u.ReasoningTokens))
	}
	if u.Cost != nil {
		est := usageCostTotal(u)
		if est > 0 || u.Cost.Input > 0 || u.Cost.Output > 0 {
			attrs = append(attrs,
				oDoubleKV("omp.gen_ai.cost.estimated_usd", est),
				oDoubleKV("omp.gen_ai.cost.input_usd", u.Cost.Input),
				oDoubleKV("omp.gen_ai.cost.output_usd", u.Cost.Output),
			)
		}
	}
	return attrs
}

func usageCostTotal(u *ompUsage) float64 {
	if u == nil || u.Cost == nil {
		return 0
	}
	if u.Cost.Total > 0 {
		return u.Cost.Total
	}
	return u.Cost.Input + u.Cost.Output + u.Cost.CacheRead + u.Cost.CacheWrite
}

func mapOmpProviderToOtel(provider string) string {
	switch provider {
	case "amazon-bedrock":
		return "aws.bedrock"
	case "google", "google-antigravity", "google-gemini-cli":
		return "gcp.gemini"
	case "google-vertex":
		return "gcp.vertex_ai"
	case "mistral":
		return "mistral_ai"
	case "openai-codex":
		return "openai"
	case "xai":
		return "x_ai"
	default:
		return provider
	}
}

func mapOmpStopReason(reason string) string {
	switch reason {
	case "stop":
		return "stop"
	case "length":
		return "length"
	case "toolUse":
		return "tool_calls"
	case "error", "aborted":
		return "error"
	default:
		return ""
	}
}

func userContentToOtelParts(raw json.RawMessage) ([]ompOtelPart, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil || strings.TrimSpace(s) == "" {
			return nil, false
		}
		return []ompOtelPart{{Type: "text", Content: s}}, true
	}
	blocks := parseOmpContentBlocks(trimmed)
	var parts []ompOtelPart
	hasContent := false
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, ompOtelPart{Type: "text", Content: blk.Text})
				hasContent = true
			}
		case "image":
			// Omit raw base64 bytes so image prompts never exceed OTLP size limits while
			// adapter/omp still counts image-only prompts via type=="blob".
			parts = append(parts, ompOtelPart{Type: "blob", Modality: "image", MimeType: blk.MimeType})
			hasContent = true
		}
	}
	return parts, hasContent
}

func assistantBlocksToOtelParts(blocks []ompContentBlock) []ompOtelPart {
	var parts []ompOtelPart
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, ompOtelPart{Type: "text", Content: blk.Text})
			}
		case "thinking":
			if blk.Thinking != "" {
				parts = append(parts, ompOtelPart{Type: "reasoning", Content: blk.Thinking})
			}
		case "toolCall":
			args := blk.Arguments
			if len(args) > ompMaxToolArgs {
				args = json.RawMessage(`{"_truncated":true}`)
			}
			parts = append(parts, ompOtelPart{Type: "tool_call", ID: blk.ID, Name: blk.Name, Arguments: args})
		}
	}
	return parts
}

func buildOmpToolResultJSON(rawContent, rawDetails json.RawMessage, details *ompToolDetails, isError bool) string {
	type textItem struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var items []textItem
	remaining := ompMaxToolText
	for _, blk := range parseOmpContentBlocks(rawContent) {
		if blk.Type != "text" || remaining <= 0 {
			continue
		}
		txt := cutUTF8(blk.Text, remaining)
		remaining -= len(txt)
		items = append(items, textItem{Type: "text", Text: txt})
	}
	if items == nil {
		items = []textItem{}
	}
	out := map[string]any{
		"content": items,
		"isError": isError,
	}
	trimmedDetails := bytes.TrimSpace(rawDetails)
	if len(trimmedDetails) > 0 && !bytes.Equal(trimmedDetails, []byte("null")) {
		if len(trimmedDetails) <= ompMaxRawDetails {
			out["details"] = json.RawMessage(trimmedDetails)
		} else if details != nil {
			// Project only small structured fields (keeping meta.source when present) when
			// raw details exceed the size cap.
			projected := *details
			projected.Meta = nil
			if len(details.Meta) > 0 {
				var m struct {
					Source *struct {
						Type  string `json:"type"`
						Value string `json:"value"`
					} `json:"source"`
				}
				if json.Unmarshal(details.Meta, &m) == nil && m.Source != nil && m.Source.Type == "path" && m.Source.Value != "" {
					if rawMeta, err := json.Marshal(m); err == nil && len(rawMeta) <= 4096 {
						projected.Meta = rawMeta
					}
				}
			}
			out["details"] = projected
		}
	}
	return marshalCompactJSON(out)
}

func parseOmpContentBlocks(raw json.RawMessage) []ompContentBlock {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []ompContentBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return nil
	}
	return blocks
}

func millisDuration(ms *float64) time.Duration {
	if ms == nil || math.IsNaN(*ms) || math.IsInf(*ms, 0) || *ms <= 0 {
		return 0
	}
	if *ms >= float64(math.MaxInt64/int64(time.Millisecond)) {
		return 0
	}
	return time.Duration(*ms * float64(time.Millisecond))
}

func assistantSpanTimes(msg *ompMessage, entryTS time.Time) (start, end time.Time) {
	dur := millisDuration(msg.Duration)
	if msg.CompletedAt > 0 {
		end = time.UnixMilli(msg.CompletedAt).UTC()
		if dur > 0 {
			start = end.Add(-dur)
		} else if msg.Timestamp > 0 {
			start = time.UnixMilli(msg.Timestamp).UTC()
		} else {
			start = end
		}
	} else if msg.Timestamp > 0 {
		// In pi-ai / OMP, msg.Timestamp is stamped at stream creation (request start) while
		// entryTS is written at persistence. Start at msg.Timestamp and cap end at entryTS so
		// timelines stay ordered while never projecting past subsequent tool results.
		start = time.UnixMilli(msg.Timestamp).UTC()
		end = start
		if dur > 0 {
			end = start.Add(dur)
		} else if !entryTS.IsZero() && entryTS.After(start) {
			end = entryTS
		}
		if !entryTS.IsZero() && end.After(entryTS) {
			end = entryTS
		}
		if end.Before(start) {
			start = end
		}
	} else if !entryTS.IsZero() {
		end = entryTS
		start = entryTS
		if dur > 0 {
			start = end.Add(-dur)
		}
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

func messageTime(tsMillis int64, fallback time.Time) time.Time {
	if tsMillis > 0 {
		return time.UnixMilli(tsMillis).UTC()
	}
	return fallback
}

func parseOmpTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t.UTC()
}

func timeNanos(t time.Time) uint64 {
	if t.IsZero() || t.UnixNano() <= 0 {
		return 0
	}
	return uint64(t.UnixNano())
}

func deriveTraceID(sessionID, key string) []byte {
	sum := sha256.Sum256([]byte("terma-omp-trace:" + sessionID + ":" + key))
	id := make([]byte, 16)
	copy(id, sum[:16])
	if bytes.Equal(id, make([]byte, 16)) {
		id[15] = 1
	}
	return id
}

func deriveSpanID(sessionID, key string) []byte {
	sum := sha256.Sum256([]byte("terma-omp-span:" + sessionID + ":" + key))
	id := make([]byte, 8)
	copy(id, sum[:8])
	if bytes.Equal(id, make([]byte, 8)) {
		id[7] = 1
	}
	return id
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

func marshalCompactJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func cutUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
