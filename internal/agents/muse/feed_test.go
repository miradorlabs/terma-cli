package muse

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// entry is a view entry as `muse serve` pages it.
func entry(t *testing.T, method string, params map[string]any) mspEvent {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return mspEvent{Method: method, Params: b}
}

// A tool call's start and finish make its duration, under the hook's call id, so it folds
// into the hook's call; a turn's finish names its outcome and timings; a rename is a title;
// what the hooks already say is not repeated.
func TestMuseViewRecords(t *testing.T) {
	started := map[string]time.Time{}
	item := func(status, at string) map[string]any {
		return map[string]any{"sessionId": museSession, "item": map[string]any{
			"itemId": "01a1c37f-217d-7d31-8ba4-545861312f17", "kind": "toolCall", "turnId": museTurn, "status": status,
			"recordedAt": at, "tool": "shell", "callId": "call_01a1c37f1ef2", "args": `{"command":"ls"}`,
		}}
	}
	if _, ok := museRecord(museSession, entry(t, "item/started", item("inProgress", "2026-11-10T03:27:40.104Z")), started); ok {
		t.Fatal("a start is no record")
	}
	rec, ok := museRecord(museSession, entry(t, "item/completed", item("completed", "2026-11-10T03:27:41.404Z")), started)
	if !ok || rec.Name != semconv.TermaToolCallEvent || rec.Attrs[semconv.GenAIToolCallIDKey] != "call_01a1c37f1ef2" ||
		rec.Attrs[semconv.TermaOperationDurationMsKey] != int64(1300) || rec.Attrs[semconv.TermaOperationStatusKey] != "completed" ||
		rec.Attrs[semconv.TermaEvidenceSourceKey] != sourceMuseSession || rec.Attrs[semconv.GenAIToolCallArgumentsKey] != nil {
		t.Fatalf("tool call = %+v, %v", rec, ok)
	}
	if len(started) != 0 {
		t.Errorf("a finished call stayed among the started: %v", started)
	}

	rec, ok = museRecord(museSession, entry(t, "turn/completed", map[string]any{"sessionId": museSession, "turnId": museTurn,
		"terminal": "completed", "durationMs": 8301, "timeToFirstTokenMs": 3250}), started)
	if !ok || rec.Name != semconv.TermaTurnEvent || rec.Attrs[semconv.TermaTurnIDKey] != museTurn ||
		rec.Attrs[semconv.TermaOperationDurationMsKey] != int64(8301) || rec.Attrs[semconv.TermaTurnTimeToFirstTokenMsKey] != int64(3250) {
		t.Fatalf("turn = %+v, %v", rec, ok)
	}

	rec, ok = museRecord(museSession, entry(t, "session/nameChanged", map[string]any{"sessionId": museSession, "name": " Fix the flaky test ", "nameOrigin": "generated"}), started)
	if !ok || rec.Name != semconv.TermaSessionTitleEvent || rec.Attrs[semconv.TermaSessionTitleKey] != "Fix the flaky test" {
		t.Fatalf("title = %+v, %v", rec, ok)
	}

	for _, ev := range []mspEvent{
		entry(t, "item/completed", map[string]any{"item": map[string]any{"kind": "userMessage", "text": "never read"}}),
		entry(t, "session/tokenUsage", map[string]any{"turnId": museTurn, "modelId": "muse-wonder-1.0", "usage": map[string]any{"inputTokens": 1}}),
		entry(t, "item/completed", map[string]any{"item": map[string]any{"kind": "toolCall", "tool": "shell", "status": "completed"}}),
	} {
		if rec, ok := museRecord(museSession, ev, started); ok {
			t.Errorf("%s spooled %+v", ev.Method, rec)
		}
	}
}

// `muse usage --json` is the plan and its two windows; resets are in seconds.
func TestMuseQuota(t *testing.T) {
	out := []byte(`{"schemaVersion":1,"allowance":{"tier":"Muse Code Power Usage","window":{"usedPercent":6,"resetsAtMs":1794296383000},"weekly":{"usedPercent":2,"resetsAtMs":1794787200000},"asOfMs":1794281529693,"source":"mint"},"reason":null,"hint":null}`)
	rec, ok := museQuota(museSession, out)
	if !ok || rec.Name != semconv.TermaSessionQuotaEvent || rec.SessionID != museSession {
		t.Fatalf("quota = %+v, %v", rec, ok)
	}
	for k, want := range map[string]any{
		semconv.TermaAccountPlanTypeKey:             "Muse Code Power Usage",
		semconv.TermaRateLimitPrimaryUsedPercentKey: int64(6), semconv.TermaRateLimitPrimaryResetsAtKey: int64(1794296383),
		semconv.TermaRateLimitSecondaryUsedPercentKey: int64(2), semconv.TermaRateLimitSecondaryResetsAtKey: int64(1794787200),
		semconv.TermaRateLimitSecondaryWindowMinutesKey: int64(10080), semconv.TermaEvidenceSourceKey: sourceMuseUsage,
	} {
		if rec.Attrs[k] != want {
			t.Errorf("%s = %v, want %v", k, rec.Attrs[k], want)
		}
	}
	if !rec.Time.Equal(time.UnixMilli(1794281529693)) {
		t.Errorf("observed at %v", rec.Time)
	}
	if _, ok := museQuota(museSession, []byte(`{"schemaVersion":1,"allowance":null,"reason":"signed_out"}`)); ok {
		t.Error("no allowance made a quota")
	}
}
