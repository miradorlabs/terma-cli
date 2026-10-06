package cursor

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// postToolUse records one finished tool call: the generic pair is the one place a
// tool_use_id and a duration arrive together. Tool inputs, outputs, error messages and
// the tool's working directory are never read.
func postToolUse(ctx context.Context, env hookrun.Env) error {
	return cursorToolCall(ctx, env, "postToolUse")
}

// postToolUseFailure is postToolUse's record for a failed call, with the failure type
// and nothing of the error's text.
func postToolUseFailure(ctx context.Context, env hookrun.Env) error {
	return cursorToolCall(ctx, env, "postToolUseFailure")
}

func cursorToolCall(ctx context.Context, env hookrun.Env, hook string) error {
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
	attrs, ok := cursorToolCallAttrs(in, hook)
	if !ok {
		env.Logf("%s names no tool and no call id", hook)
		return nil
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: in.id(), Attrs: attrs})
	return nil
}

// cursorToolCallAttrs is the event body, false for a payload naming neither a tool nor a
// call; failure types stay in Cursor's words, and no account email rides on a call.
func cursorToolCallAttrs(in *cursorHookInput, hook string) (map[string]any, bool) {
	a := hookrun.EvidenceAttrs(cursorTool, sourceCursorHook, hook)
	if hookrun.ShortLabel(in.ToolName) {
		a[semconv.GenAIToolNameKey] = in.ToolName
	}
	if cursorCallID(in.ToolUseID) {
		a[semconv.GenAIToolCallIDKey] = in.ToolUseID
	}
	if _, named := a[semconv.GenAIToolNameKey]; !named {
		if _, identified := a[semconv.GenAIToolCallIDKey]; !identified {
			return nil, false
		}
	}
	for k, v := range map[string]string{semconv.TermaTurnIDKey: in.GenerationID, semconv.GenAIRequestModelKey: in.Model, semconv.GenAIResponseModelKey: in.ModelID, semconv.TermaMainAgentVersionKey: in.CursorVersion} {
		hookrun.BoundedAttr(a, k, v)
	}
	copyModelParams(in, a)
	// Missing stays missing; a value not a non-negative integer is invalid, not repaired.
	if value, present, ok := hookrun.JSONNumber(in.Duration, true); ok {
		a[semconv.TermaOperationDurationMsKey] = int64(value)
	} else if present {
		a[semconv.TermaOperationDurationStatusKey] = semconv.TermaOperationDurationStatusInvalid
	}
	switch hook {
	case "postToolUse":
		a[semconv.TermaOperationStatusKey] = "completed"
	case "postToolUseFailure":
		a[semconv.TermaOperationStatusKey] = "error"
		switch in.FailureType {
		case "error", "timeout", "permission_denied":
			a[semconv.ErrorTypeKey] = in.FailureType
		default:
			a[semconv.ErrorTypeKey] = semconv.ErrorTypeOther
		}
		if b, ok := cursorBool(in.IsInterrupt); ok {
			a[semconv.TermaOperationInterruptedKey] = b
		}
	}
	return a, true
}

// cursorCallID admits a tool_use_id of one printable token up to 256 bytes; as a replay
// key it is rejected, never trimmed.
func cursorCallID(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func cursorBool(raw json.RawMessage) (bool, bool) {
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
