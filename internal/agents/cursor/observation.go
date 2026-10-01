package cursor

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// These handlers record observations, never additive counters: Cursor may repeat a token
// snapshot at afterAgentResponse and stop, omit it, or exclude subagents.

func beforeSubmitPrompt(ctx context.Context, env hookrun.Env) error {
	return cursorObserve(ctx, env, "beforeSubmitPrompt")
}

func afterAgentResponse(ctx context.Context, env hookrun.Env) error {
	return cursorObserve(ctx, env, "afterAgentResponse")
}

func stop(ctx context.Context, env hookrun.Env) error { return cursorObserve(ctx, env, "stop") }

func preCompact(ctx context.Context, env hookrun.Env) error {
	return cursorObserve(ctx, env, "preCompact")
}

func cursorObserve(ctx context.Context, env hookrun.Env, hook string) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if hook == "beforeSubmitPrompt" {
		// A conversation can outlive the active TTL, and the CLI may omit sessionStart.
		env.SetActive(r, env.NewSession(r, in.id(), cursorTool, in.Model))
	}
	captureCursorObservation(ctx, env, r, in, hook)
	return nil
}

func cursorObservationAttrs(in *cursorHookInput, hook string) map[string]any {
	a := hookrun.EvidenceAttrs(cursorTool, sourceCursorHook, hook)
	for _, k := range []string{"funding_status", "quota_status", "account_status"} {
		a[k] = hookrun.StatusUnavailable
	}
	for k, v := range map[string]string{hookrun.AttrTurnID: in.GenerationID, hookrun.AttrModel: in.Model, "model_id": in.ModelID, "cursor.version": in.CursorVersion, "provider_session_id": in.SessionID, "account_email": in.UserEmail} {
		hookrun.BoundedAttr(a, k, v)
	}
	if _, ok := a["account_email"]; ok {
		a["account_status"] = "available"
	}
	cursorModelParams(in, a)
	switch hook {
	case "afterAgentResponse", "stop":
		n, invalid := 0, false
		for k, v := range map[string]json.RawMessage{"input_tokens": in.InputTokens, "output_tokens": in.OutputTokens, "cache_read_tokens": in.CacheReadTokens, "cache_write_tokens": in.CacheWriteTokens} {
			value, present, ok := hookrun.JSONNumber(v, true)
			if present && !ok {
				invalid = true
			}
			if ok {
				a["reported_"+k] = int64(value)
				n++
			}
		}
		status := hookrun.StatusUnavailable
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
				a[hookrun.AttrStatus] = in.Status
			default:
				a[hookrun.AttrStatus] = hookrun.UnknownValue
			}
			if v, _, ok := hookrun.JSONNumber(in.LoopCount, true); ok {
				a["loop_count"] = int64(v)
			}
		}
	case "preCompact":
		// Context occupancy is neither an allowance nor a usage delta.
		for k, v := range map[string]json.RawMessage{"context_tokens": in.ContextTokens, "context_window_size": in.ContextWindowSize, "context_usage_percent": in.ContextUsagePercent} {
			if value, _, ok := hookrun.JSONNumber(v, k != "context_usage_percent"); ok {
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

// captureCursorObservation records one Cursor hook as an ordered observation.
func captureCursorObservation(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *cursorHookInput, hook string) {
	e.CaptureObservation(ctx, r, hookrun.Observation{
		Tool: cursorTool, Source: sourceCursorHook, StateDir: cursorObservationDir,
		SessionID: in.id(), Hook: hook, TurnID: in.GenerationID, Attrs: cursorObservationAttrs(in, hook),
	})
}
