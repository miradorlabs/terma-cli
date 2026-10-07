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
// so a session whose hooks were trusted mid-way is still forwarded; it names the policy
// the session was claimed under.
func ClaimFromPayload(ctx context.Context, env Env, s PayloadSession, tool string) (config.Policy, bool) {
	if s.ID == "" {
		return config.Policy{}, false
	}
	id := s.ID
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
	var c claim.Claim
	var under config.Policy
	global, isGlobal := env.global()
	switch r, err := env.Repo(ctx); {
	case err == nil && r.ProjectID != "":
		c = claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree, Repository: r.Repository, Root: r.workTree()}
		under = r.Policy
	case errors.Is(err, ErrNotAdmitted):
		for _, sid := range []string{id, s.AgentID} {
			if sid != "" {
				notCollected(env, sid, tool)
			}
		}
		refreshPolicy(env)
		return config.Policy{}, false
	case err != nil && isGlobal && global.DefaultProjectID != "":
		// Global mode, outside any repository.
		c = claim.Claim{ProjectID: global.DefaultProjectID, Tool: tool, Repo: filepath.Base(env.Cwd)}
		under = global
	default:
		return config.Policy{}, false
	}
	c.PIDs, c.Cwd = claimPIDs(), env.workingDir()
	// A subagent's telemetry may name its own id (see claimForRelay).
	if s.AgentID != "" && s.AgentID != id {
		claim.Write(env.StateDir, s.AgentID, c, env.Time())
	}
	if !claim.Write(env.StateDir, id, c, env.Time()) {
		prev, live := claim.Read(env.StateDir, id, env.Time())
		return under, live && prev.ProjectID != ""
	}
	return under, true
}

// workingDir is the directory the hook ran in, symlink-resolved as Repo.workTree is: where
// in the checkout the agent works, which a tool call's relative paths resolve against.
func (e Env) workingDir() string {
	if e.Cwd == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(e.Cwd); err == nil {
		return resolved
	}
	return e.Cwd
}

// notCollected marks a session running in a working copy the team policy does not admit
// (an agent's cwd can change per turn), so the relay drops what its processes send from now
// on instead of holding it; earlier records keep their own placement. A policy not yet
// validated is no answer, so it marks only a session already claimed or marked.
func notCollected(env Env, sessionID, tool string) {
	if !env.policies().Validated() {
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

// refreshPolicy starts the detached flush when a policy the machine collects under is
// stale; the flush refreshes every stale one first. With no relay running, nothing else
// learns of a repository a team has since listed, and the next hook would mark its
// sessions too. Of hooks racing past a team's stamp, the one holding its lock starts the flush.
func refreshPolicy(env Env) {
	if env.Flush == nil {
		return
	}
	due := false
	for _, p := range env.policies() {
		if p.Stale(env.Time()) && stampRefresh(env, p.Team()) {
			due = true
		}
	}
	if due {
		env.Flush()
	}
}

// stampRefresh records that team's refresh is being started now, false when one was
// within refreshEvery or another hook holds the stamp.
func stampRefresh(env Env, team string) bool {
	if team == "" || team != filepath.Base(team) {
		return false
	}
	stamp, now := filepath.Join(env.StateDir, config.PoliciesDir, refreshedDir, team), env.Time()
	if os.MkdirAll(filepath.Dir(stamp), 0o700) != nil {
		return false
	}
	unlock, err := flock.TryLock(stamp + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < refreshEvery {
		return false
	}
	return os.WriteFile(stamp, nil, 0o600) == nil && os.Chtimes(stamp, now, now) == nil
}

// claimPIDs are this hook's ancestors, one of them the agent: a claim covers only their records.
var claimPIDs = sync.OnceValue(procinfo.Ancestors)
