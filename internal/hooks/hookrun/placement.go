package hookrun

import (
	"cmp"
	"context"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func (e Env) repoClaim(r *Repo, tool string) claim.Claim {
	c := r.claim(tool)
	c.Cwd, c.PIDs, c.Parent = e.workingDir(), claimPIDs(), e.parent
	return c
}

func claimRouted(ctx context.Context, env Env, s PayloadSession, tool string) bool {
	if !env.Routing.Usable() || env.lane().Lost != "" {
		return false
	}
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
	r, err := env.routeHere(ctx)
	if err != nil {
		return false
	}
	if r.dest.collected() {
		env.unpark(r)
	}
	env.placeRun(s.ID, s.AgentID, env.repoClaim(r, tool), env.Time())
	return r.dest.collected()
}

func (e Env) placeFor(r *Repo, ev spool.Event, claimAgent bool) {
	if r == nil || r.dest == nil || ev.SessionID == "" || e.lane().Lost != "" {
		return
	}
	tool, _ := ev.Attrs[semconv.GenAIMainAgentNameKey].(string)
	var agent string
	if claimAgent {
		agent, _ = ev.Attrs[semconv.TermaAgentIDKey].(string)
	}
	e.placeRun(ev.SessionID, agent, e.repoClaim(r, tool), e.Time())
	if e.OnClaim != nil && r.dest.collected() {
		e.OnClaim(r.Root)
	}
}

func (e Env) placeRun(sessionID, agentID string, c claim.Claim, now time.Time) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) || e.Routing.WithholdPlacements || e.lane().Lost != "" {
		return
	}
	c.ProjectID = ""
	c.Profile, c.Account = e.Routing.Profile, e.claimAccount()
	claim.Write(e.StateDir, sessionID, c, now)
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) {
		claim.Write(e.StateDir, agentID, c, now)
	}
}
