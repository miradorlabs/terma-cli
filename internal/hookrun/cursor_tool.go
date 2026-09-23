package hookrun

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

// CursorPostToolUse and CursorPostToolUseFailure record one finished tool call.
//
// Cursor's generic postToolUse pair fires for every tool type — Shell, Read, Write,
// Grep, Delete, Task and MCP:<tool> alike — and is the one place a tool_use_id and a
// duration arrive together, so it is the pair wired. The before* hooks are permission
// gates on the critical path of every call: a hook process before each tool runs,
// nothing observational once the post hook carries the duration, and a fail-open
// default that a schema mismatch or `failClosed` would turn into a blocked action on a
// machine where terma misbehaves. afterShellExecution and afterMCPExecution restate
// calls postToolUse already reported, without a tool_use_id and with the command output.
// Tool inputs, outputs, error messages and the tool's working directory are never read.
func CursorPostToolUse(ctx context.Context, env Env) error {
	return cursorToolCall(ctx, env, "postToolUse")
}

// CursorPostToolUseFailure handles a tool call that failed or was interrupted: the same
// record as CursorPostToolUse, with the failure type and nothing of the error's text.
func CursorPostToolUseFailure(ctx context.Context, env Env) error {
	return cursorToolCall(ctx, env, "postToolUseFailure")
}

func cursorToolCall(ctx context.Context, env Env, hook string) error {
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
	attrs, ok := cursorToolCallAttrs(in, hook)
	if !ok {
		env.logf("%s names no tool and no call id", hook)
		return nil
	}
	attrs[attrVersion] = env.Version
	env.emitFor(r, spool.Event{Name: EventToolCall, SessionID: in.id(), Repo: repoName(r.root), Attrs: attrs})
	return nil
}

// cursorToolCallAttrs is the event body: identifiers, the tool's name, its timing and
// its outcome. A payload naming neither a tool nor a call is not a tool call terma can
// describe and is dropped. Cursor's failure vocabulary is kept as its own words; the
// platform translates. The account email does not ride on a tool call — a call is not a
// principal record, and the session already says who was signed in.
func cursorToolCallAttrs(in *cursorHookInput, hook string) (map[string]any, bool) {
	a := evidenceAttrs(cursorTool, sourceCursorHook, hook)
	if shortLabel(in.ToolName) {
		a[attrToolName] = in.ToolName
	}
	if cursorCallID(in.ToolUseID) {
		a[attrToolCallID] = in.ToolUseID
	}
	if _, named := a[attrToolName]; !named {
		if _, identified := a[attrToolCallID]; !identified {
			return nil, false
		}
	}
	for k, v := range map[string]string{attrTurnID: in.GenerationID, attrModel: in.Model, "model_id": in.ModelID, "cursor.version": in.CursorVersion} {
		boundedAttr(a, k, v)
	}
	cursorModelParams(in, a)
	// Cursor reports the tool's execution time in milliseconds. Missing stays missing;
	// a value that is not a non-negative integer is reported as invalid, not repaired.
	if value, present, ok := cursorNumber(in.Duration, true); ok {
		a["duration_ms"] = int64(value)
	} else if present {
		a["duration_status"] = "invalid"
	}
	switch hook {
	case "postToolUse":
		a[attrStatus] = "completed"
	case "postToolUseFailure":
		a[attrStatus] = "error"
		switch in.FailureType {
		case "error", "timeout", "permission_denied":
			a["failure_type"] = in.FailureType
		default:
			a["failure_type"] = unknownValue
		}
		if b, ok := cursorBool(in.IsInterrupt); ok {
			a["is_interrupt"] = b
		}
	}
	return a, true
}

// cursorCallID admits a tool_use_id: one token, printable, at most 256 bytes. The id is
// an opaque identifier that becomes a replay key, so whitespace and control characters
// are rejected rather than trimmed.
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

// cursorBool reads an optional boolean; anything but true or false is absent.
func cursorBool(raw json.RawMessage) (bool, bool) {
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
