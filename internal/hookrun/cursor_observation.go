package hookrun

import (
	"context"
	"encoding/json"
	"math"
)

// The four handlers below record observations, not additive usage counters. Cursor may
// repeat the same parent-turn token snapshot at afterAgentResponse and stop, omit it, or
// exclude subagents. Never derive billing or quota from these numbers.

// CursorBeforeSubmitPrompt observes a prompt being submitted, and refreshes the active
// session: an IDE conversation can outlive its TTL, and a CLI client may never have
// sent sessionStart.
func CursorBeforeSubmitPrompt(ctx context.Context, env Env) error {
	return cursorObserve(ctx, env, "beforeSubmitPrompt")
}

// CursorAfterAgentResponse observes the end of a model response, which is where Cursor
// reports a token snapshot when it reports one at all.
func CursorAfterAgentResponse(ctx context.Context, env Env) error {
	return cursorObserve(ctx, env, "afterAgentResponse")
}

// CursorStop observes the end of a turn. The committed entry sets loop_limit to null so
// it keeps firing past Cursor's five follow-up loops.
func CursorStop(ctx context.Context, env Env) error { return cursorObserve(ctx, env, "stop") }

// CursorPreCompact observes a context compaction, the one moment context occupancy is
// reported — which is not a billing quota.
func CursorPreCompact(ctx context.Context, env Env) error {
	return cursorObserve(ctx, env, "preCompact")
}

func cursorObserve(ctx context.Context, env Env, hook string) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	if hook == "beforeSubmitPrompt" {
		// IDE conversations can outlive the active manifest's TTL; CLI clients may
		// omit sessionStart. A submitted prompt refreshes attribution in both cases.
		env.setActive(r, env.newSession(r, in.id(), cursorTool, in.Model))
	}
	env.captureCursorObservation(ctx, r, in, hook)
	return nil
}

func cursorObservationAttrs(in *cursorHookInput, hook string) map[string]any {
	a := evidenceAttrs(cursorTool, sourceCursorHook, hook)
	for _, k := range []string{"funding_status", "quota_status", "account_status"} {
		a[k] = statusUnavailable
	}
	for k, v := range map[string]string{attrTurnID: in.GenerationID, attrModel: in.Model, "model_id": in.ModelID, "cursor.version": in.CursorVersion, "provider_session_id": in.SessionID, "account_email": in.UserEmail} {
		boundedAttr(a, k, v)
	}
	if _, ok := a["account_email"]; ok {
		a["account_status"] = "available"
	}
	cursorModelParams(in, a)
	switch hook {
	case "afterAgentResponse", "stop":
		n, invalid := 0, false
		for k, v := range map[string]json.RawMessage{"input_tokens": in.InputTokens, "output_tokens": in.OutputTokens, "cache_read_tokens": in.CacheReadTokens, "cache_write_tokens": in.CacheWriteTokens} {
			value, present, ok := cursorNumber(v, true)
			if present && !ok {
				invalid = true
			}
			if ok {
				a["reported_"+k] = int64(value)
				n++
			}
		}
		status := statusUnavailable
		if n > 0 {
			status = "partial"
		}
		if n == 4 {
			status = "available"
		}
		if invalid {
			status = "invalid"
		}
		a["usage_status"], a["usage_semantics"], a["usage_scope"] = status, "snapshot", "parent_turn"
		if hook == "stop" {
			switch in.Status {
			case "completed", "aborted", "error":
				a[attrStatus] = in.Status
			default:
				a[attrStatus] = unknownValue
			}
			if v, _, ok := cursorNumber(in.LoopCount, true); ok {
				a["loop_count"] = int64(v)
			}
		}
	case "preCompact":
		// Context occupancy is not a subscription allowance or a token usage delta.
		for k, v := range map[string]json.RawMessage{"context_tokens": in.ContextTokens, "context_window_size": in.ContextWindowSize, "context_usage_percent": in.ContextUsagePercent} {
			if value, _, ok := cursorNumber(v, k != "context_usage_percent"); ok {
				a[k] = value
			}
		}
		switch in.Trigger {
		case "auto", "manual":
			a["trigger"] = in.Trigger
		}
	}
	return a
}

func cursorNumber(raw json.RawMessage, integer bool) (float64, bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, false
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 || (integer && math.Trunc(value) != value) {
		return 0, true, false
	}
	return value, true, true
}

// captureCursorObservation records one Cursor hook as an ordered observation. The
// checkpoint directory and the observation id seed are Cursor's own, so state written
// by earlier versions keeps its sequence.
func (e Env) captureCursorObservation(ctx context.Context, r *repo, in *cursorHookInput, hook string) {
	e.captureObservation(ctx, r, observation{
		tool: cursorTool, source: sourceCursorHook, stateDir: cursorObservationDir,
		sessionID: in.id(), hook: hook, turnID: in.GenerationID, attrs: cursorObservationAttrs(in, hook),
	})
}
