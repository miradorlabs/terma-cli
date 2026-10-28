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
	b, _ := json.Marshal(map[string]any{"session_id": titleSession, "cwd": env.Cwd, "transcript_path": path, "hook_event_name": "Stop"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
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
	calls, offset, err := readCallResponses(path, 0)
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
	calls, _, err := readCallResponses(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].ServiceTier != "standard" || calls[0].InferenceGeo != "global" || calls[0].ThinkingTokens == nil || *calls[0].ThinkingTokens != 76 {
		t.Fatalf("got %+v, want req_a keeping its tier, region and count", calls)
	}
	spaced := transcript(t, t.TempDir(),
		`{"type": "assistant", "requestId": "req_s", "message": {"model": "claude-opus-5-5", "stop_reason": "end_turn", "usage": {"output_tokens": 9}}}`,
	)
	if calls, _, err := readCallResponses(spaced, 0); err != nil || len(calls) != 1 || calls[0].StopReason != "end_turn" {
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
	calls, _, err := readCallResponses(path, 0)
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
	first, offset, err := readCallResponses(path, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("first read: %+v, %v", first, err)
	}
	partial := assistantRecord("req_c", "2026-10-28T03:31:47Z", 20)[:20]
	appendTranscript(t, path, assistantRecord("req_b", "2026-10-28T03:31:46Z", 0)+"\n"+partial)
	second, next, err := readCallResponses(path, offset)
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
	calls, _, err := readCallResponses(path, 1<<20)
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
