package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type contractFailures []string

func (f *contractFailures) Helper() {}
func (f *contractFailures) Errorf(format string, args ...any) {
	*f = append(*f, fmt.Sprintf(format, args...))
}

func TestContractNumbers(t *testing.T) {
	for _, raw := range []any{nil, "", "unknown", true, "NaN", "+Inf", "-Inf", math.NaN(), -1.0, 100.1} {
		var failures contractFailures
		checkNumber(&failures, "used_pct", raw, 0, 100, false)
		if len(failures) == 0 {
			t.Errorf("accepted invalid percentage %v", raw)
		}
	}
	for _, raw := range []any{0.0, "0", 42.5, "42.5", json.Number("100"), int64(100)} {
		checkNumber(t, "used_pct", raw, 0, 100, false)
	}
	var failures contractFailures
	checkPositive(&failures, "tokens", "1.5", true)
	if len(failures) == 0 {
		t.Fatal("accepted fractional tokens")
	}
}

// Synthetic completed-call evidence: cache-only input is valid, as is a zero
// quota percentage. These are contracts, not claimed provider observations.
func claudeContractFixture(t *testing.T) (Event, []LogRecord) {
	t.Helper()
	var q Event
	err := json.Unmarshal([]byte(`{"time":"2026-09-15T10:00:00Z","session_id":"session-a","attrs":{
		"five_hour_used_pct":0,"seven_day_used_pct":100,
		"five_hour_resets_at":1789470000,"seven_day_resets_at":1790071200,
		"session_cost_usd":0.001,"prompt_id":"prompt-a"}}`), &q)
	if err != nil {
		t.Fatal(err)
	}
	return q, []LogRecord{{Attrs: map[string]string{
		"session.id": "session-a", "prompt.id": "prompt-a", "request_id": "request-a", "model": "test-model",
		"input_tokens": "0", "cache_read_tokens": "10", "cache_creation_tokens": "0",
		"output_tokens": "1", "cost_usd": "0.001", "speed": "normal",
	}}}
}

func TestClaudeValueContracts(t *testing.T) {
	q, reqs := claudeContractFixture(t)
	checkClaudeRequests(t, reqs)
	checkClaudeQuota(t, q, reqs)
	q.Attrs["five_hour_used_pct"] = 107.0 // Live-observed over-limit usage; retain it.
	checkClaudeQuota(t, q, reqs)
	// OTLP strings and JSON numbers must mean the same thing. An unrelated
	// first request must not break a join to a later request for this prompt.
	for k, v := range q.Attrs {
		q.Attrs[k] = fmt.Sprint(v)
	}
	checkClaudeQuota(t, q, append([]LogRecord{{Attrs: map[string]string{"prompt.id": "other"}}}, reqs...))

	cases := []struct {
		name          string
		breakEvidence func(*Event, []LogRecord)
		want          string
	}{
		{"missing quota", func(q *Event, _ []LogRecord) { delete(q.Attrs, "five_hour_used_pct") }, "five_hour_used_pct"},
		{"out of range", func(q *Event, _ []LogRecord) { q.Attrs["seven_day_used_pct"] = -1.0 }, "seven_day_used_pct"},
		{"expired reset", func(q *Event, _ []LogRecord) { q.Attrs["five_hour_resets_at"] = q.Time.Unix() }, "reset must be after"},
		{"missing timestamp", func(q *Event, _ []LogRecord) { q.Time = time.Time{} }, "missing observation timestamp"},
		{"zero session cost", func(q *Event, _ []LogRecord) { q.Attrs["session_cost_usd"] = 0.0 }, "session_cost_usd"},
		{"wrong prompt", func(q *Event, _ []LogRecord) { q.Attrs["prompt_id"] = "wrong" }, "no api_request"},
		{"empty prompt", func(q *Event, r []LogRecord) { q.Attrs["prompt_id"] = ""; r[0].Attrs["prompt.id"] = "" }, "no api_request"},
		{"wrong session", func(q *Event, _ []LogRecord) { q.SessionID = "session-b" }, "no api_request"},
		{"zero input", func(_ *Event, r []LogRecord) { r[0].Attrs["cache_read_tokens"] = "0" }, "total_input_tokens"},
		{"zero output", func(_ *Event, r []LogRecord) { r[0].Attrs["output_tokens"] = "0" }, "output_tokens"},
		{"negative cache", func(_ *Event, r []LogRecord) { r[0].Attrs["cache_read_tokens"] = "-1" }, "cache_read_tokens"},
		{"zero cost", func(_ *Event, r []LogRecord) { r[0].Attrs["cost_usd"] = "0" }, "cost_usd"},
		{"nonfinite cost", func(_ *Event, r []LogRecord) { r[0].Attrs["cost_usd"] = "NaN" }, "cost_usd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, r := claudeContractFixture(t)
			tc.breakEvidence(&q, r)
			var failures contractFailures
			checkClaudeRequests(&failures, r)
			checkClaudeQuota(&failures, q, r)
			if !strings.Contains(strings.Join(failures, "\n"), tc.want) {
				t.Fatalf("want failure containing %q, got %v", tc.want, failures)
			}
		})
	}
}

func TestCodexValueContracts(t *testing.T) {
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	fixture := func() (map[string]any, []LogRecord) {
		limits := map[string]any{"plan_type": "team"}
		for _, name := range []string{"primary", "secondary"} {
			limits[name] = map[string]any{"used_percent": 0.0, "window_minutes": 300.0, "resets_at": at.Add(time.Hour).Unix()}
		}
		return limits, []LogRecord{{Attrs: map[string]string{
			"input_token_count": "10", "output_token_count": "1", "cached_token_count": "0",
		}}}
	}
	limits, completed := fixture()
	checkCodexRateLimits(t, limits, at)
	checkCodexCompleted(t, completed)
	for _, field := range []string{"used_percent", "window_minutes", "resets_at"} {
		t.Run("invalid "+field, func(t *testing.T) {
			limits, _ := fixture()
			limits["primary"].(map[string]any)[field] = -1.0
			var failures contractFailures
			checkCodexRateLimits(&failures, limits, at)
			if !strings.Contains(strings.Join(failures, "\n"), field) {
				t.Fatalf("accepted invalid %s: %v", field, failures)
			}
		})
	}
	for _, field := range []string{"input_token_count", "output_token_count", "cached_token_count"} {
		t.Run("invalid "+field, func(t *testing.T) {
			_, completed := fixture()
			completed[0].Attrs[field] = "0"
			if field == "cached_token_count" {
				completed[0].Attrs[field] = "11" // Cached input cannot exceed total input.
			}
			var failures contractFailures
			checkCodexCompleted(&failures, completed)
			if !strings.Contains(strings.Join(failures, "\n"), field) {
				t.Fatalf("accepted invalid %s: %v", field, failures)
			}
		})
	}
}

func TestQuotaTimestampSurvivesDelivery(t *testing.T) {
	q, reqs := claudeContractFixture(t)
	q.Name = "terma.session.quota"
	r := &Receiver{}
	// Exercise OTLP decoding without a listener or provider credential.
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"terma-cli"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","attributes":[
		{"key":"event.name","value":{"stringValue":"terma.session.quota"}},
		{"key":"session.id","value":{"stringValue":"session-a"}}
	]}]}]}]}`, q.Time.UnixNano())
	req := httptest.NewRequest("POST", "/v1/logs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.handleLogs(response, req)
	if response.Code != 200 || len(r.Logs()) != 1 {
		t.Fatalf("OTLP decode: %d %s", response.Code, response.Body.String())
	}
	for k, v := range q.Attrs {
		r.logs[0].Attrs[k] = fmt.Sprint(v)
	}
	sb := &Sandbox{TermaConfig: t.TempDir(), Receiver: r}
	// A newer snapshot is still queued while an older one has been delivered.
	newer := q
	newer.Time = q.Time.Add(time.Minute)
	data, _ := json.Marshal(newer)
	if err := os.MkdirAll(filepath.Join(sb.TermaConfig, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.TermaConfig, "spool", "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	events := sb.Events(q.Name, q.SessionID)
	if len(events) != 2 || !events[0].Time.Equal(q.Time) || !events[1].Time.Equal(newer.Time) {
		t.Fatalf("lost observation times or ordering: %+v", events)
	}
	// The fixed fixture's reset may be past by test time; delivery delay must
	// not invalidate a window that was in the future when observed.
	checkClaudeQuota(t, events[0], reqs)
}
