package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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

// ReadPayloadSession reads the session_id, agent_id and cwd most agents' payloads share.
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

// ClaimFromPayload claims a hook's session for the relay when its handler spooled nothing,
// so a session whose hooks were trusted mid-way is still forwarded.
func ClaimFromPayload(ctx context.Context, env Env, s PayloadSession, tool string) bool {
	if s.ID == "" || !claim.Enabled() {
		return false
	}
	id := s.ID
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
	var c claim.Claim
	switch r, err := env.Repo(ctx); {
	case err == nil && r.ProjectID != "":
		c = claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree, Repository: r.Repository}
	case errors.Is(err, ErrNotAdmitted):
		for _, sid := range []string{id, s.AgentID} {
			if sid != "" {
				withdraw(ctx, env, sid)
			}
		}
		return false
	case err != nil && env.Policy.Global() && env.Policy.DefaultProjectID != "":
		// Global mode, outside any repository.
		c = claim.Claim{ProjectID: env.Policy.DefaultProjectID, Tool: tool, Repo: filepath.Base(env.Cwd)}
	default:
		return false
	}
	c.PIDs = claimPIDs()
	// A subagent's telemetry may name its own id (see claimForRelay).
	if s.AgentID != "" && s.AgentID != id {
		claim.Write(s.AgentID, c, env.Time())
	}
	if !claim.Write(id, c, env.Time()) {
		_, live := claim.Read(id, env.Time())
		return live
	}
	return true
}

// withdraw moves a claimed session, now running in a working copy the team policy does
// not admit (an agent's cwd can change per turn), to a placement there, so the relay drops
// what its processes send from now on; earlier records keep their own placement.
func withdraw(ctx context.Context, env Env, sessionID string) {
	prev, ok := claim.Read(sessionID, env.Time())
	if !ok {
		return
	}
	_, _, id, err := env.locate(ctx)
	if err != nil {
		return
	}
	claim.Write(sessionID, claim.Claim{ProjectID: prev.ProjectID, Repository: id, PIDs: claimPIDs()}, env.Time())
}

// claimPIDs are this hook's ancestors, one of them the agent: a claim covers only their records.
var claimPIDs = sync.OnceValue(procinfo.Ancestors)
