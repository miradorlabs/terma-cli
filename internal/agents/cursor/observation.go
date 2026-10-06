package cursor

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
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
	for _, k := range []string{semconv.TermaFundingStatusKey, semconv.TermaQuotaStatusKey, semconv.TermaAccountStatusKey} {
		a[k] = hookrun.StatusUnavailable
	}
	for k, v := range map[string]string{semconv.TermaTurnIDKey: in.GenerationID, semconv.GenAIRequestModelKey: in.Model, semconv.GenAIResponseModelKey: in.ModelID, semconv.TermaMainAgentVersionKey: in.CursorVersion, semconv.TermaProviderSessionIDKey: in.SessionID, semconv.UserEmailKey: in.UserEmail} {
		hookrun.BoundedAttr(a, k, v)
	}
	if _, ok := a[semconv.UserEmailKey]; ok {
		a[semconv.TermaAccountStatusKey] = "available"
	}
	copyModelParams(in, a)
	switch hook {
	case "afterAgentResponse", "stop":
		n, invalid := 0, false
		for k, v := range map[string]json.RawMessage{
			semconv.TermaUsageInputTokensKey: in.InputTokens, semconv.TermaUsageOutputTokensKey: in.OutputTokens,
			semconv.TermaUsageCacheReadTokensKey: in.CacheReadTokens, semconv.TermaUsageCacheWriteTokensKey: in.CacheWriteTokens,
		} {
			value, present, ok := hookrun.JSONNumber(v, true)
			if present && !ok {
				invalid = true
			}
			if ok {
				a[k] = int64(value)
				n++
			}
		}
		status := semconv.TermaUsageStatusUnavailable
		if n > 0 {
			status = semconv.TermaUsageStatusPartial
		}
		if n == 4 {
			status = semconv.TermaUsageStatusAvailable
		}
		if invalid {
			status = semconv.TermaUsageStatusInvalid
		}
		a[semconv.TermaUsageStatusKey], a[semconv.TermaUsageSemanticsKey], a[semconv.TermaUsageScopeKey] = status, semconv.TermaUsageSemanticsSnapshot, semconv.TermaUsageScopeParentTurn
		if hook == "stop" {
			switch in.Status {
			case "completed", "aborted", "error":
				a[semconv.TermaOperationStatusKey] = in.Status
			default:
				a[semconv.TermaOperationStatusKey] = hookrun.UnknownValue
			}
			if v, _, ok := hookrun.JSONNumber(in.LoopCount, true); ok {
				a[semconv.TermaLoopCountKey] = int64(v)
			}
		}
	case "preCompact":
		// Context occupancy is neither an allowance nor a usage delta.
		for k, v := range map[string]json.RawMessage{semconv.TermaContextTokensKey: in.ContextTokens, semconv.TermaContextWindowSizeKey: in.ContextWindowSize} {
			if value, _, ok := hookrun.JSONNumber(v, true); ok {
				a[k] = int64(value)
			}
		}
		if value, _, ok := hookrun.JSONNumber(in.ContextUsagePercent, false); ok {
			a[semconv.TermaContextUsedPercentKey] = value
		}
		switch in.Trigger {
		case semconv.TermaCompactionTriggerAuto, semconv.TermaCompactionTriggerManual:
			a[semconv.TermaCompactionTriggerKey] = in.Trigger
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
