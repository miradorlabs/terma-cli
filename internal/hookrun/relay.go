package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// ClaimFromPayload claims a hook's session for the local relay from the payload the
// agent sent, for the hooks whose handler spooled nothing: a Codex tool call that
// edited no file, a Stop with nothing new in the rollout. Those are exactly the hooks
// of a session whose hooks were trusted mid-way, and without a claim none of its
// telemetry would be forwarded. emitFor claims everything that is spooled; this is
// the net under it. tool is the agent's label (ToolForEvent).
func ClaimFromPayload(ctx context.Context, env Env, payload []byte, tool string) bool {
	if len(payload) == 0 || !claim.Enabled() {
		return false
	}
	var in struct {
		SessionID      string   `json:"session_id"`
		ConversationID string   `json:"conversation_id"`
		Antigravity    string   `json:"conversationId"`
		Cwd            string   `json:"cwd"`
		Workspaces     []string `json:"workspacePaths"`
	}
	if json.Unmarshal(payload, &in) != nil {
		return false
	}
	id := cmp.Or(in.SessionID, in.ConversationID, in.Antigravity)
	if id == "" {
		return false
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	if len(in.Workspaces) > 0 && in.Workspaces[0] != "" {
		env.Cwd = in.Workspaces[0]
	}
	r, err := env.repo(ctx)
	if err != nil || r.projectID == "" {
		return false
	}
	if !claim.Write(id, claim.Claim{ProjectID: r.projectID, Tool: tool, Repo: r.name, Worktree: r.worktree}, env.now()) {
		_, live := claim.Read(id, env.now())
		return live
	}
	return true
}

// ToolForEvent is the agent label a `terma hook <event>` name belongs to — the same
// labels the trailers and spooled events carry.
func ToolForEvent(event string) string {
	for prefix, tool := range map[string]string{
		"codex-": codexTool, "cursor-": cursorTool, "antigravity-": antigravityTool, "opencode-": opencodeTool,
	} {
		if strings.HasPrefix(event, prefix) {
			return tool
		}
	}
	return claudeTool
}
