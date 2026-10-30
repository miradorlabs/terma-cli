package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// assistantRecord is a final transcript record for one of a call's content blocks.
func assistantRecord(requestID, at string, thinking int) string {
	return `{"type":"assistant","requestId":"` + requestID + `","timestamp":"` + at + `","effort":"high",` +
		`"message":{"model":"claude-opus-5-5","stop_reason":"tool_use","content":[],"usage":{"output_tokens":1308,` +
		`"service_tier":"standard","inference_geo":"global","output_tokens_details":{"thinking_tokens":` + strconv.Itoa(thinking) + `}}}}`
}

func appendTranscript(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func spooledResponses(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == semconv.TermaModelCallResponseEvent {
			out = append(out, e)
		}
	}
	return out
}

func stopClaude(t *testing.T, env hookrun.Env, path string) {
	t.Helper()
	runClaudeHook(t, env, stop, map[string]any{"transcript_path": path, "hook_event_name": "Stop"})
}

// runClaudeHook runs a Claude hook handler on the title session with the given input.
func runClaudeHook(t *testing.T, env hookrun.Env, handler func(context.Context, hookrun.Env) error, in map[string]any) {
	t.Helper()
	in["session_id"], in["cwd"] = titleSession, env.Cwd
	b, _ := json.Marshal(in)
	env.Stdin = strings.NewReader(string(b))
	if err := handler(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// A call's last final record holds its response; the call is timed by its first, and a record
// written before the call stopped is not final.
func TestReadCallResponsesKeepsEachCallsLastFinalRecord(t *testing.T) {
	path := transcript(t, t.TempDir(),
		`{"type":"user","message":{"content":"hi"}}`,
		`{"type":"assistant","requestId":"req_a","timestamp":"2026-10-28T03:31:44Z","message":{"model":"claude-opus-5-5","stop_reason":null,"usage":{"output_tokens":243,"output_tokens_details":{"thinking_tokens":40}}}}`,
		assistantRecord("req_a", "2026-10-28T03:31:45.100Z", 76),
		assistantRecord("req_a", "2026-10-28T03:31:46.200Z", 76),
		assistantRecord("req_b", "2026-10-28T03:31:50Z", 0),
	)
	calls, _, offset, err := readTranscriptFacts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %+v, want req_a and req_b", calls)
	}
	a := calls[0]
	if a.RequestID != "req_a" || a.ThinkingTokens == nil || *a.ThinkingTokens != 76 || a.Model != "claude-opus-5-5" ||
		a.StopReason != "tool_use" || a.ServiceTier != "standard" || a.InferenceGeo != "global" ||
		!a.Time.Equal(time.Date(2026, 10, 28, 3, 31, 45, 100e6, time.UTC)) {
		t.Errorf("req_a: %+v", a)
	}
	if b := calls[1]; b.RequestID != "req_b" || b.ThinkingTokens == nil || *b.ThinkingTokens != 0 {
		t.Errorf("req_b: %+v, want a reported zero", b)
	}
	if info, _ := os.Stat(path); offset != info.Size() {
		t.Errorf("offset %d, want the transcript's size %d", offset, info.Size())
	}
}

// A final record written with spaces is still read, and one that omits a label keeps the
// label an earlier final record gave.
func TestReadCallResponsesToleratesSpacingAndOmittedLabels(t *testing.T) {
	path := transcript(t, t.TempDir(),
		assistantRecord("req_a", "2026-10-28T03:31:45Z", 76),
		`{"type": "assistant", "requestId": "req_a", "message": {"model": "claude-opus-5-5", "stop_reason": "tool_use", "usage": {"output_tokens": 1308}}}`,
	)
	calls, _, _, err := readTranscriptFacts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].ServiceTier != "standard" || calls[0].InferenceGeo != "global" || calls[0].ThinkingTokens == nil || *calls[0].ThinkingTokens != 76 {
		t.Fatalf("got %+v, want req_a keeping its tier, region and count", calls)
	}
	spaced := transcript(t, t.TempDir(),
		`{"type": "assistant", "requestId": "req_s", "message": {"model": "claude-opus-5-5", "stop_reason": "end_turn", "usage": {"output_tokens": 9}}}`,
	)
	if calls, _, _, err := readTranscriptFacts(spaced, 0); err != nil || len(calls) != 1 || calls[0].StopReason != "end_turn" {
		t.Fatalf("spaced record: got %+v, %v", calls, err)
	}
}

// A build that records no thinking count still reports the response; an unreported region is
// left out, and Claude Code's own synthetic replies are not calls.
func TestReadCallResponsesWithoutAThinkingCount(t *testing.T) {
	path := transcript(t, t.TempDir(),
		`{"type":"assistant","requestId":"req_old","message":{"model":"claude-opus-4-5","stop_reason":"end_turn","usage":{"output_tokens":90,"service_tier":"standard","inference_geo":"not_available"}}}`,
		`{"type":"assistant","requestId":"req_syn","message":{"model":"<synthetic>","stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
	)
	calls, _, _, err := readTranscriptFacts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].RequestID != "req_old" || calls[0].ThinkingTokens != nil || calls[0].InferenceGeo != "" || calls[0].StopReason != "end_turn" {
		t.Fatalf("got %+v, want req_old with no count and no region", calls)
	}
}

// Reading resumes after the last whole line; a line still being written waits for the next read.
func TestReadCallResponsesResumesAfterTheLastWholeLine(t *testing.T) {
	path := transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 5))
	first, _, offset, err := readTranscriptFacts(path, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first read: %+v, %v", first, err)
	}
	partial := assistantRecord("req_c", "2026-10-28T03:31:47Z", 20)[:20]
	appendTranscript(t, path, assistantRecord("req_b", "2026-10-28T03:31:46Z", 0)+"\n"+partial)
	second, _, next, err := readTranscriptFacts(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].RequestID != "req_b" {
		t.Fatalf("second read: got %+v, want only req_b", second)
	}
	if info, _ := os.Stat(path); next != info.Size()-int64(len(partial)) {
		t.Errorf("offset %d stops inside the partial line, want %d", next, info.Size()-int64(len(partial)))
	}
}

// A transcript shorter than the stored offset is read from its start.
func TestReadCallResponsesRestartsAShorterTranscript(t *testing.T) {
	path := transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 5))
	calls, _, _, err := readTranscriptFacts(path, 1<<20)
	if err != nil || len(calls) != 1 {
		t.Fatalf("got %+v, %v; want req_a", calls, err)
	}
}

// Stop spools one response per call, carrying only labels and counts, and a later stop spools
// only the calls recorded since.
func TestClaudeStopSpoolsEachCallsResponseOnce(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 76))
	stopClaude(t, env, path)
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 {
		t.Fatalf("expected one response, got %+v", got)
	}
	e := got[0]
	want := map[string]any{
		semconv.GenAIMainAgentNameKey:              claudeTool,
		semconv.TermaEvidenceSourceKey:             sourceClaudeTranscript,
		semconv.TermaModelCallIDKey:                "req_a",
		semconv.GenAIResponseFinishReasonsKey:      []any{"tool_use"},
		semconv.GenAIResponseModelKey:              "claude-opus-5-5",
		semconv.GenAIUsageReasoningOutputTokensKey: float64(76),
		semconv.TermaResponseServiceTierKey:        "standard",
		semconv.TermaResponseInferenceGeoKey:       "global",
		hookrun.AttrProjectID:                      e.Attrs[hookrun.AttrProjectID],
	}
	gotJSON, _ := json.Marshal(e.Attrs)
	wantJSON, _ := json.Marshal(want)
	if e.SessionID != titleSession || string(gotJSON) != string(wantJSON) {
		t.Fatalf("response %s in %s, want %s", gotJSON, e.SessionID, wantJSON)
	}
	if !e.Time.Equal(time.Date(2026, 10, 28, 3, 31, 45, 0, time.UTC)) {
		t.Errorf("response timed %v, want the call's first record", e.Time)
	}

	appendTranscript(t, path, assistantRecord("req_b", "2026-10-28T03:32:00Z", 12)+"\n")
	stopClaude(t, env, path)
	got = spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaModelCallIDKey] != "req_b" {
		t.Fatalf("second stop: got %+v, want req_b alone", got)
	}
}

// A developer who did not choose Claude Code at setup sends no responses.
func TestClaudeStopSendsNoResponsesWithoutConsent(t *testing.T) {
	env := titleEnv(t)
	env.Agents = nil
	path := transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 76))
	stopClaude(t, env, path)
	if got := spooledResponses(t, env.Spool); len(got) != 0 {
		t.Fatalf("expected no responses, got %+v", got)
	}
}

// A subagent's responses come from its own transcript, in the parent's session, when it stops.
func TestClaudeSubagentStopSpoolsItsResponses(t *testing.T) {
	env := titleEnv(t)
	ctx := context.Background()
	agentPath := filepath.Join(t.TempDir(), "agent-a44816aa66a297cdd.jsonl")
	hookruntest.WriteFile(t, filepath.Dir(agentPath), filepath.Base(agentPath), assistantRecord("req_sub", "2026-10-28T03:31:45Z", 300)+"\n")
	common := `"session_id":"` + titleSession + `","cwd":"` + env.Cwd + `","agent_id":"a44816aa66a297cdd","agent_type":"Explore"`
	env.Stdin = strings.NewReader(`{` + common + `,"hook_event_name":"SubagentStart"}`)
	if err := subagentStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader(`{` + common + `,"hook_event_name":"SubagentStop","agent_transcript_path":` + jsonString(agentPath) + `}`)
	if err := subagentStop(ctx, env); err != nil {
		t.Fatal(err)
	}
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].SessionID != titleSession || got[0].Attrs[semconv.TermaModelCallIDKey] != "req_sub" {
		t.Fatalf("got %+v, want req_sub in the parent session", got)
	}
}

// A call still streaming at one stop is sent, with its final count, at the next.
func TestClaudeStopSendsAStreamingCallOnceItIsFinal(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(),
		`{"type":"assistant","requestId":"req_a","timestamp":"2026-10-28T03:31:44Z","message":{"model":"claude-opus-5-5","stop_reason":null,"usage":{"output_tokens":243,"output_tokens_details":{"thinking_tokens":40}}}}`,
	)
	stopClaude(t, env, path)
	if got := spooledResponses(t, env.Spool); len(got) != 0 {
		t.Fatalf("a streaming call was sent: %+v", got)
	}
	appendTranscript(t, path, assistantRecord("req_a", "2026-10-28T03:31:45Z", 76)+"\n")
	stopClaude(t, env, path)
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaModelCallIDKey] != "req_a" || got[0].Attrs[semconv.GenAIUsageReasoningOutputTokensKey] != float64(76) {
		t.Fatalf("got %+v, want req_a with its final count", got)
	}
}

// A turn that fails after a completed call has no Stop: its failure sends the call, and the
// session's end does not send it again.
func TestClaudeStopFailureSendsTheTurnsCompletedCalls(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(),
		assistantRecord("req_a", "2026-10-28T03:31:45Z", 76),
		`{"type":"assistant","sessionId":"`+titleSession+`","timestamp":"2026-10-28T03:31:50Z","isApiErrorMessage":true,"error":"server_error","message":{"model":"<synthetic>","stop_reason":"stop_sequence","usage":{"output_tokens":0}}}`,
	)
	runClaudeHook(t, env, stopFailure, map[string]any{"transcript_path": path, "hook_event_name": "StopFailure", "error": "server_error"})
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaModelCallIDKey] != "req_a" {
		t.Fatalf("got %+v, want req_a at the failure", got)
	}
	runClaudeHook(t, env, sessionEnd, map[string]any{"transcript_path": path, "hook_event_name": "SessionEnd"})
	if again := spooledResponses(t, env.Spool); len(again) != 0 {
		t.Fatalf("the session's end sent a call again: %+v", again)
	}
}

// A final record written after the turn's Stop read the transcript is sent when the session ends.
func TestClaudeSessionEndSendsACallFinishedAfterStop(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(),
		`{"type":"assistant","requestId":"req_a","timestamp":"2026-10-28T03:31:44Z","message":{"model":"claude-opus-5-5","stop_reason":null,"usage":{"output_tokens":243,"output_tokens_details":{"thinking_tokens":40}}}}`,
	)
	stopClaude(t, env, path)
	if got := spooledResponses(t, env.Spool); len(got) != 0 {
		t.Fatalf("a streaming call was sent: %+v", got)
	}
	appendTranscript(t, path, assistantRecord("req_a", "2026-10-28T03:31:45Z", 76)+"\n")
	runClaudeHook(t, env, sessionEnd, map[string]any{"transcript_path": path, "hook_event_name": "SessionEnd"})
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaModelCallIDKey] != "req_a" || got[0].Attrs[semconv.GenAIUsageReasoningOutputTokensKey] != float64(76) {
		t.Fatalf("got %+v, want req_a with its final count at the session's end", got)
	}
}

// fallbackRecord is the system record of a refusal that moved the session to another model,
// with the text the record carries beside its labels.
func fallbackRecord(requestID, scope string, sidechain bool) string {
	return `{"type":"system","subtype":"model_refusal_fallback","level":"warning","isMeta":false,` +
		`"isSidechain":` + strconv.FormatBool(sidechain) + `,"requestId":"` + requestID + `","timestamp":"2026-10-26T09:12:03Z",` +
		`"originalModel":"claude-opus-5-5","fallbackModel":"claude-opus-4-8","trigger":"refusal","scope":"` + scope + `",` +
		`"direction":"retry","apiRefusalCategory":"cyber","apiRefusalExplanation":"SECRET-EXPLANATION",` +
		`"content":"SECRET-CONTENT","refusedUserMessageUuid":"u-1","retractedMessageUuids":["u-2"]}`
}

func spooledFallbacks(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == semconv.TermaModelFallbackEvent {
			out = append(out, e)
		}
	}
	return out
}

// The main thread's session fallback is read beside the calls; a subagent's, one scoped to less
// than the session, and one naming no refused call are not.
func TestReadTranscriptFactsReadsTheSessionsFallbacks(t *testing.T) {
	path := transcript(t, t.TempDir(),
		assistantRecord("req_a", "2026-10-26T09:12:00Z", 5),
		fallbackRecord("req_refused", "session", false),
		fallbackRecord("req_sub", "session", true),
		fallbackRecord("req_turn", "turn", false),
		fallbackRecord("", "session", false),
	)
	calls, fallbacks, _, err := readTranscriptFacts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].RequestID != "req_a" {
		t.Errorf("calls %+v, want req_a alone", calls)
	}
	want := []claudeModelFallback{{RequestID: "req_refused", From: "claude-opus-5-5", To: "claude-opus-4-8", Trigger: "refusal",
		Category: "cyber", Time: time.Date(2026, 10, 26, 9, 12, 3, 0, time.UTC)}}
	if len(fallbacks) != 1 || fallbacks[0] != want[0] {
		t.Errorf("fallbacks %+v, want %+v", fallbacks, want)
	}
}

// Stop spools the fallback once, with its labels only: never the record's text or the API's
// explanation.
func TestClaudeStopSpoolsAFallbackOnceWithoutItsText(t *testing.T) {
	env := titleEnv(t)
	path := transcript(t, t.TempDir(), fallbackRecord("req_refused", "session", false))
	stopClaude(t, env, path)
	all := hookruntest.Spooled(t, env.Spool)
	var got []spool.Event
	for _, e := range all {
		if b, _ := json.Marshal(e); strings.Contains(string(b), "SECRET") {
			t.Fatalf("the record's text left in %s", b)
		}
		if e.Name == semconv.TermaModelFallbackEvent {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected one fallback, got %+v", all)
	}
	e := got[0]
	want := map[string]any{
		semconv.GenAIMainAgentNameKey:        claudeTool,
		semconv.GenAIRequestModelKey:         "claude-opus-5-5",
		semconv.TermaEvidenceSourceKey:       sourceClaudeTranscript,
		semconv.TermaModelCallIDKey:          "req_refused",
		semconv.TermaModelFallbackModelKey:   "claude-opus-4-8",
		semconv.TermaModelFallbackTriggerKey: "refusal",
		semconv.TermaRefusalCategoryKey:      "cyber",
		hookrun.AttrProjectID:                e.Attrs[hookrun.AttrProjectID],
	}
	gotJSON, _ := json.Marshal(e.Attrs)
	wantJSON, _ := json.Marshal(want)
	if e.SessionID != titleSession || string(gotJSON) != string(wantJSON) || !e.Time.Equal(time.Date(2026, 10, 26, 9, 12, 3, 0, time.UTC)) {
		t.Fatalf("fallback %s in %s at %v, want %s at the record's time", gotJSON, e.SessionID, e.Time, wantJSON)
	}

	appendTranscript(t, path, assistantRecord("req_b", "2026-10-26T09:13:00Z", 12)+"\n")
	stopClaude(t, env, path)
	if again := spooledFallbacks(t, env.Spool); len(again) != 0 {
		t.Fatalf("a later stop sent the fallback again: %+v", again)
	}
}
