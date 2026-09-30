package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/miradorlabs/terma-cli/internal/procinfo"

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
		AgentID        string   `json:"agent_id"`
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
	var c claim.Claim
	switch r, err := env.Repo(ctx); {
	case err == nil && r.ProjectID != "":
		c = claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree}
	case err != nil && env.Policy.Global() && env.Policy.DefaultProjectID != "":
		// Global mode, outside any repository: a scratch directory, the home directory.
		c = claim.Claim{ProjectID: env.Policy.DefaultProjectID, Tool: tool, Repo: filepath.Base(env.Cwd)}
	default:
		return false
	}
	c.PIDs = claimPIDs()
	// A Codex subagent's telemetry names its own thread (see claimForRelay).
	if in.AgentID != "" && in.AgentID != id {
		claim.Write(in.AgentID, c, env.Time())
	}
	if !claim.Write(id, c, env.Time()) {
		_, live := claim.Read(id, env.Time())
		return live
	}
	return true
}

// claimPIDs are the processes this hook runs under, one of which is the agent: a claim
// covers only records those processes export. Walked once per hook.
var claimPIDs = sync.OnceValue(procinfo.Ancestors)

// ToolForEvent is the agent label a `terma hook <event>` name belongs to — the same
// labels the trailers and spooled events carry.
func ToolForEvent(event string) string {
	for prefix, tool := range map[string]string{
		"codex-": codexTool, "cursor-": cursorTool, "antigravity-": antigravityTool, "opencode-": opencodeTool,
		"omp-": ompTool, "pi-": piTool, "hermes-": hermesTool, "gemini-": geminiTool, "dsh-": dshTool,
	} {
		if strings.HasPrefix(event, prefix) {
			return tool
		}
	}
	return claudeTool
}

// UserPromptSubmit is Claude Code's turn-start hook. terma records nothing for it:
// the caller claims the session for the local relay from the payload this reads, and
// starts the relay, so a relay that died between turns is back before the turn's
// telemetry is exported. It must print nothing — Claude Code hands this hook's stdout
// to the model as context.
func UserPromptSubmit(_ context.Context, env Env) error {
	_, err := readHookInput[struct {
		SessionID string `json:"session_id"`
	}](env.Stdin)
	return err
}
