package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"strconv"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// permissionRequest records an approval request; Codex reports no decision to
// repository hooks, so it is neither an approval nor a denial.
func permissionRequest(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if !codexDesktopRoute(env, r) {
		return nil
	}
	// The hook input names only the turn; the backend groups a turn's events by trace id (#100).
	cursor := captureCodexDesktopActivity(ctx, env, r, in)
	at := env.Time()
	attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexHook, "PermissionRequest")
	attrs[semconv.TermaCaptureSurfaceKey] = semconv.TermaCaptureSurfaceDesktop
	attrs[semconv.TermaObservationIDKey] = hookrun.EvidenceID(in.SessionID + "|" + in.TurnID + "|" + in.ToolName + "|" + strconv.FormatInt(at.UnixNano(), 10))
	hookrun.BoundedAttr(attrs, semconv.TermaTurnIDKey, in.TurnID)
	hookrun.BoundedAttr(attrs, semconv.GenAIToolNameKey, in.ToolName)
	hookrun.BoundedAttr(attrs, semconv.TermaApprovalPermissionModeKey, in.PermissionMode)
	var input struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(in.ToolInput, &input) == nil {
		hookrun.BoundedAttr(attrs, semconv.TermaApprovalReasonKey, input.Description)
	}
	ev := spool.Event{Time: at, Name: semconv.TermaApprovalRequestedEvent, SessionID: in.SessionID, Attrs: attrs}
	if cursor.TurnID == in.TurnID {
		ev.TraceID = cursor.TraceID
	}
	env.EmitFor(r, ev)
	return nil
}
