package antigravity

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// sourceAntigravityTranscript is terma.evidence.source for what agy's transcript recorded.
const sourceAntigravityTranscript = "antigravity_transcript"

// antigravitySubagentMarker opens the result of invoke_subagent, which names the child
// conversation the subagent runs in.
const antigravitySubagentMarker = "Created the following subagent:"

// antigravityCallEvent is one PLANNER_RESPONSE as a terma.model_call. agy counts input
// without the cache reads, which OpenTelemetry's input count includes, and thinking
// inside output: no reasoning count is sent, and the usage is partial, rather than a zero.
func antigravityCallEvent(id string, s antigravityStep, turn string) spool.Event {
	step := *s.StepIndex
	a := map[string]any{
		semconv.GenAIMainAgentNameKey:  antigravityTool,
		semconv.TermaEvidenceSourceKey: sourceAntigravityTranscript,
		semconv.TermaModelCallIDKey:    "step-" + strconv.FormatInt(step, 10),
		semconv.TermaStepIndexKey:      step,
	}
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, s.Model)
	if hookrun.ShortLabel(turn) {
		a[semconv.TermaTurnIDKey] = turn
	}
	switch s.Status {
	case "DONE":
		a[semconv.TermaOperationStatusKey] = "completed"
	case "ERROR":
		a[semconv.TermaOperationStatusKey] = "error"
	}
	input, inPresent, inOK := hookrun.JSONNumber(s.InputTokens, true)
	cached, cachePresent, cacheOK := hookrun.JSONNumber(s.CacheReadTokens, true)
	output, outPresent, outOK := hookrun.JSONNumber(s.OutputTokens, true)
	switch {
	case (inPresent && !inOK) || (cachePresent && !cacheOK) || (outPresent && !outOK):
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusInvalid
	case !inPresent && !cachePresent && !outPresent:
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusUnavailable
	default:
		// Thinking and cache writes are never counted apart.
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusPartial
		if inPresent {
			// A missing cache count is no cache read: cached is then 0.
			a[semconv.GenAIUsageInputTokensKey] = int64(input + cached)
		}
		if cachePresent {
			a[semconv.GenAIUsageCacheReadInputTokensKey] = int64(cached)
		}
		if outPresent {
			a[semconv.GenAIUsageOutputTokensKey] = int64(output)
		}
	}
	return spool.Event{Name: semconv.TermaModelCallEvent, SessionID: id, Attrs: a}
}

// antigravityLineTime is when agy wrote a line, now when it does not say or says a time later
// than now, the hook's real time: no line was written after the hook that reads it.
func antigravityLineTime(s antigravityStep, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil && !t.After(now) {
		return t
	}
	return now
}

// antigravityLineAttrs are what every event of a transcript line carries.
func antigravityLineAttrs(s antigravityStep, turn string) map[string]any {
	a := map[string]any{
		semconv.GenAIMainAgentNameKey:  antigravityTool,
		semconv.TermaEvidenceSourceKey: sourceAntigravityTranscript,
		semconv.TermaStepIndexKey:      *s.StepIndex,
	}
	if hookrun.ShortLabel(turn) {
		a[semconv.TermaTurnIDKey] = turn
	}
	return a
}

// antigravityStepID is the id a step's line goes under: its index, which only grows within a
// conversation, so a line read twice goes under one id.
func antigravityStepID(s antigravityStep) string {
	return "step-" + strconv.FormatInt(*s.StepIndex, 10)
}

// antigravityPromptEvent is one USER_INPUT as a terma.user.message: always sent, so a prompt
// counts, with its text only when withText and the split of T3 Code's wrapper is sure.
func antigravityPromptEvent(id string, s antigravityStep, turn string, withText bool) spool.Event {
	a := antigravityLineAttrs(s, turn)
	a[semconv.TermaMessageIDKey] = antigravityStepID(s)
	if !withText {
		return spool.Event{Name: semconv.TermaUserMessageEvent, SessionID: id, Attrs: a}
	}
	if text, ok := antigravityPrompt(s.Content); ok {
		a[semconv.TermaMessageTextKey], a[semconv.TermaMessageTruncatedKey] = cutText(text)
	}
	return spool.Event{Name: semconv.TermaUserMessageEvent, SessionID: id, Attrs: a}
}

// antigravityReplyEvent is a PLANNER_RESPONSE's content as a terma.assistant.message, at the
// same step as its terma.model_call.
func antigravityReplyEvent(id string, s antigravityStep, turn string) spool.Event {
	a := antigravityLineAttrs(s, turn)
	a[semconv.TermaMessageIDKey] = antigravityStepID(s)
	a[semconv.TermaMessageTextKey], a[semconv.TermaMessageTruncatedKey] = cutText(s.Content)
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, s.Model)
	return spool.Event{Name: semconv.TermaAssistantMessageEvent, SessionID: id, Attrs: a}
}

// antigravityResultEvent is a GENERIC line as a terma.tool.result: the tool call id is the
// step's, the one its PostToolUse names, so the platform joins it to the hook's call. The
// line names no tool.
func antigravityResultEvent(id string, s antigravityStep, turn string) spool.Event {
	a := antigravityLineAttrs(s, turn)
	a[semconv.GenAIToolCallIDKey] = antigravityStepID(s)
	a[semconv.GenAIToolCallResultKey] = cutResult(s.Content)
	return spool.Event{Name: semconv.TermaToolResultEvent, SessionID: id, Attrs: a}
}

// antigravitySubagent is the child conversation an invoke_subagent result names, or "".
func antigravitySubagent(content string) string {
	_, rest, ok := strings.Cut(content, antigravitySubagentMarker)
	if !ok {
		return ""
	}
	var child struct {
		ConversationID string `json:"conversationId"`
	}
	if json.NewDecoder(strings.NewReader(rest)).Decode(&child) != nil || !session.ValidID(child.ConversationID) {
		return ""
	}
	return child.ConversationID
}
