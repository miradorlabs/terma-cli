package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// preToolUse records a local start time for PostToolUse's elapsed wall time, which
// includes approval waits and is not Codex's execution duration.
func preToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) || in.ToolUseID == "" {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if !codexDesktopRoute(r) {
		return nil
	}
	path, err := codexToolStartPath(in)
	if err != nil {
		return nil
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = hookrun.WriteState(path, []byte(strconv.FormatInt(env.Time().UnixNano(), 10)))
	}
	return nil
}

func codexToolStartPath(in *codexHookInput) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexToolStartDir, hookrun.EvidenceID(in.SessionID+"|"+in.ToolUseID)+".json"), nil
}

func codexToolElapsed(e hookrun.Env, in *codexHookInput) (int64, bool) {
	if in.ToolUseID == "" {
		return 0, false
	}
	path, err := codexToolStartPath(in)
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	_ = os.Remove(path)
	start, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	d := e.Time().Sub(time.Unix(0, start))
	if d < 0 || d > 24*time.Hour {
		return 0, false
	}
	return d.Milliseconds(), true
}

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
	if !codexDesktopRoute(r) {
		return nil
	}
	at := env.Time()
	attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexHook, "PermissionRequest")
	attrs["capture_surface"] = codexDesktopSurface
	attrs["observation_id"] = hookrun.EvidenceID(in.SessionID + "|" + in.TurnID + "|" + in.ToolName + "|" + strconv.FormatInt(at.UnixNano(), 10))
	hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, in.TurnID)
	hookrun.BoundedAttr(attrs, hookrun.AttrToolName, in.ToolName)
	hookrun.BoundedAttr(attrs, "permission_mode", in.PermissionMode)
	var input struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(in.ToolInput, &input) == nil {
		hookrun.BoundedAttr(attrs, hookrun.AttrReason, input.Description)
	}
	env.EmitFor(r, spool.Event{Time: at, Name: hookrun.EventApprovalAsked, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}
