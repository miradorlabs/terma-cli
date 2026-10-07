package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
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
	if s.ID == "" {
		return false
	}
	id := s.ID
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
	var c claim.Claim
	switch r, err := env.Repo(ctx); {
	case err == nil && r.ProjectID != "":
		c = claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree, Repository: r.Repository, Root: r.workTree()}
	case errors.Is(err, ErrNotAdmitted):
		for _, sid := range []string{id, s.AgentID} {
			if sid != "" {
				notCollected(env, sid, tool)
			}
		}
		if env.Policy.Stale(env.Time()) {
			refreshPolicy(env)
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
		claim.Write(env.StateDir, s.AgentID, c, env.Time())
	}
	if !claim.Write(env.StateDir, id, c, env.Time()) {
		prev, live := claim.Read(env.StateDir, id, env.Time())
		return live && prev.ProjectID != ""
	}
	return true
}

// notCollected marks a session running in a working copy the team policy does not admit
// (an agent's cwd can change per turn), so the relay drops what its processes send from now
// on instead of holding it; earlier records keep their own placement. A policy not yet
// validated is no answer, so it marks only a session already claimed or marked.
func notCollected(env Env, sessionID, tool string) {
	if !env.Policy.Validated() {
		if _, ok := claim.Read(env.StateDir, sessionID, env.Time()); !ok {
			return
		}
	}
	claim.Mark(env.StateDir, sessionID, tool, claimPIDs(), env.Time())
}

// refreshedDir is the policies' folder of one file per team whose age keeps hooks to one
// policy refresh per refreshEvery.
const (
	refreshedDir = "refreshed"
	refreshEvery = time.Minute
)

// refreshPolicy starts the detached flush, which refreshes a stale policy first: with no
// relay running, nothing else learns of a repository the team has since listed, and the
// next hook would mark its sessions too. Of hooks racing past the stamp, the one holding
// its lock starts the flush.
func refreshPolicy(env Env) {
	team := env.Policy.Team()
	if env.Flush == nil || team == "" || team != filepath.Base(team) {
		return
	}
	stamp, now := filepath.Join(env.StateDir, config.PoliciesDir, refreshedDir, team), env.Time()
	if os.MkdirAll(filepath.Dir(stamp), 0o700) != nil {
		return
	}
	unlock, err := flock.TryLock(stamp + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < refreshEvery {
		return
	}
	if err := os.WriteFile(stamp, nil, 0o600); err != nil || os.Chtimes(stamp, now, now) != nil {
		return
	}
	env.Flush()
}

// claimPIDs are this hook's ancestors, one of them the agent: a claim covers only their records.
var claimPIDs = sync.OnceValue(procinfo.Ancestors)
