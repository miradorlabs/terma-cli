package live

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// A small reporter lets offline fixtures prove that malformed evidence fails
// the same checks used against real harnesses.
type contractReporter interface {
	Helper()
	Errorf(string, ...any)
}

// Spool and rollout JSON decode numbers as float64; the receiver flattens OTLP
// values to strings. Do not turn absent, null, boolean or malformed data into 0.
func checkNumber(t contractReporter, label string, raw any, min, max float64, integer bool) float64 {
	t.Helper()
	var text string
	switch v := raw.(type) {
	case float64:
		text = strconv.FormatFloat(v, 'g', -1, 64)
	case int64:
		text = strconv.FormatInt(v, 10)
	case string:
		text = v
	case json.Number:
		text = string(v)
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min || n > max || (integer && math.Trunc(n) != n) {
		t.Errorf("%s = %v: want a finite %snumber in [%g, %g]", label, raw, map[bool]string{true: "whole ", false: ""}[integer], min, max)
		return math.NaN()
	}
	return n
}

func checkPositive(t contractReporter, label string, raw any, integer bool) float64 {
	t.Helper()
	n := checkNumber(t, label, raw, 0, math.Inf(1), integer)
	if n == 0 {
		t.Errorf("%s = 0: a completed reply must report positive usage", label)
	}
	return n
}

func checkFutureReset(t contractReporter, label string, raw any, at time.Time) {
	t.Helper()
	n := checkNumber(t, label, raw, 0, math.MaxInt64, true)
	if at.IsZero() {
		t.Errorf("%s: missing observation timestamp", label)
	} else if n <= float64(at.Unix()) {
		t.Errorf("%s = %v: reset must be after observation at %s", label, raw, at.Format(time.RFC3339Nano))
	}
}

func checkClaudeRequests(t contractReporter, reqs []LogRecord) {
	t.Helper()
	for i, r := range reqs {
		label := fmt.Sprintf("api_request[%d]", i)
		var input float64
		for _, k := range []string{"input_tokens", "cache_read_tokens", "cache_creation_tokens"} {
			input += checkNumber(t, label+"."+k, r.Attrs[k], 0, math.Inf(1), true)
		}
		// Claude's cache buckets are separate input categories; an entirely
		// cached prompt need not report any uncached input_tokens.
		checkPositive(t, label+".total_input_tokens", input, true)
		checkPositive(t, label+".output_tokens", r.Attrs["output_tokens"], true)
		checkPositive(t, label+".cost_usd", r.Attrs["cost_usd"], false)
		for _, k := range []string{"session.id", "prompt.id", "request_id", "model"} {
			if strings.TrimSpace(r.Attrs[k]) == "" {
				t.Errorf("%s.%s is empty", label, k)
			}
		}
		if speed := r.Attrs["speed"]; speed != "normal" && speed != "fast" {
			t.Errorf("%s.speed = %q", label, speed)
		}
	}
}

func checkClaudeQuota(t contractReporter, quota Event, reqs []LogRecord) {
	t.Helper()
	for _, window := range []string{"five_hour", "seven_day", "spend_limit"} {
		_, used := quota.Attrs[window+"_used_pct"]
		_, reset := quota.Attrs[window+"_resets_at"]
		if window == "spend_limit" && !used && !reset {
			continue // Only gateways expose this window.
		}
		checkNumber(t, "quota."+window+"_used_pct", quota.Attrs[window+"_used_pct"], 0, math.Inf(1), false)
		checkFutureReset(t, "quota."+window+"_resets_at", quota.Attrs[window+"_resets_at"], quota.Time)
	}
	checkPositive(t, "quota.session_cost_usd", quota.Attrs["session_cost_usd"], false)
	prompt, _ := quota.Attrs["prompt_id"].(string)
	if strings.TrimSpace(prompt) != "" && quota.SessionID != "" {
		// A prompt can produce multiple requests. Join within this session,
		// without assuming request arrival order or a one-to-one relationship.
		for _, r := range reqs {
			if r.Attrs["session.id"] == quota.SessionID && r.Attrs["prompt.id"] == prompt {
				return
			}
		}
	}
	t.Errorf("quota prompt_id %q has no api_request with matching prompt.id in session %q", prompt, quota.SessionID)
}

func checkCodexCompleted(t contractReporter, completed []LogRecord) {
	t.Helper()
	var totalInput, totalOutput float64
	for i, r := range completed {
		label := fmt.Sprintf("response.completed[%d]", i)
		input := checkNumber(t, label+".input_token_count", r.Attrs["input_token_count"], 0, math.Inf(1), true)
		totalInput += input
		totalOutput += checkNumber(t, label+".output_token_count", r.Attrs["output_token_count"], 0, math.Inf(1), true)
		// Codex's cached count is a subset of input, unlike Claude's buckets.
		checkNumber(t, label+".cached_token_count", r.Attrs["cached_token_count"], 0, input, true)
	}
	checkPositive(t, "turn.input_token_count", totalInput, true)
	checkPositive(t, "turn.output_token_count", totalOutput, true)
}

func checkCodexRateLimits(t contractReporter, limits map[string]any, at time.Time) {
	t.Helper()
	if plan, _ := limits["plan_type"].(string); strings.TrimSpace(plan) == "" {
		t.Errorf("rollout.plan_type is empty")
	}
	for _, name := range []string{"primary", "secondary"} {
		window, ok := limits[name].(map[string]any)
		if !ok {
			t.Errorf("rollout.%s: want a window object, got %v", name, limits[name])
			continue
		}
		checkNumber(t, "rollout."+name+".used_percent", window["used_percent"], 0, math.Inf(1), false)
		checkPositive(t, "rollout."+name+".window_minutes", window["window_minutes"], true)
		checkFutureReset(t, "rollout."+name+".resets_at", window["resets_at"], at)
	}
}

func checkCodexDeliveredQuota(t contractReporter, record LogRecord) {
	t.Helper()
	a := record.Attrs
	if a["evidence_status"] != "present" || a["evidence_source"] != "codex_rollout" || a["session.id"] == "" {
		t.Errorf("Codex quota evidence incomplete: %v", a)
		return
	}
	at, err := time.Parse(time.RFC3339Nano, a["source_time"])
	if err != nil {
		t.Errorf("Codex quota source_time: %v", err)
	}
	limits := map[string]any{"plan_type": a["plan_type"]}
	for _, name := range []string{"primary", "secondary"} {
		limits[name] = map[string]any{"used_percent": a[name+"_used_pct"], "window_minutes": a[name+"_window_minutes"], "resets_at": a[name+"_resets_at"]}
	}
	checkCodexRateLimits(t, limits, at)
	for _, key := range []string{"has_credits", "credits_unlimited"} {
		if a[key] != "true" && a[key] != "false" {
			t.Errorf("Codex quota %s missing or not boolean: %q", key, a[key])
		}
	}
}
