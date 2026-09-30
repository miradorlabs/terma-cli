package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"sync"

	"github.com/miradorlabs/terma-cli/internal/procinfo"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// PayloadSession is what a hook payload says about its session.
type PayloadSession struct {
	ID string
	// AgentID is a subagent's own id where the agent gives it one.
	AgentID string
	// Cwd is the directory the session works in, when the payload names one.
	Cwd string
}

// ReadPayloadSession reads the fields most agents' payloads share: session_id,
// agent_id and cwd.
func ReadPayloadSession(payload []byte) (PayloadSession, bool) {
	var in struct {
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
		Cwd       string `json:"cwd"`
	}
	if json.Unmarshal(payload, &in) != nil || in.SessionID == "" {
		return PayloadSession{}, false
	}
	return PayloadSession{ID: in.SessionID, AgentID: in.AgentID, Cwd: in.Cwd}, true
}

// ClaimFromPayload claims a hook's session for the local relay from what its payload
// said, for the hooks whose handler spooled nothing: a tool call that edited no file, a
// Stop with nothing new in the rollout. Those are exactly the hooks of a session whose
// hooks were trusted mid-way, and without a claim none of its telemetry would be
// forwarded. EmitFor claims everything that is spooled; this is the net under it. tool
// is the agent's label.
func ClaimFromPayload(ctx context.Context, env Env, s PayloadSession, tool string) bool {
	if s.ID == "" || !claim.Enabled() {
		return false
	}
	id := s.ID
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
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
	if s.AgentID != "" && s.AgentID != id {
		claim.Write(s.AgentID, c, env.Time())
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
