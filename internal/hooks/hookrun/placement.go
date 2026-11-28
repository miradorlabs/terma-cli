package hookrun

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// ErrPlacementFailed signals that a hook could not persist or verify the session's placement
// on disk, so a caller checking exit status fails closed rather than exporting under an
// earlier placement.
var ErrPlacementFailed = errors.New("placement not persisted")

func (e Env) repoClaim(r *Repo, tool string) claim.Claim {
	c := r.claim(tool)
	c.Cwd, c.PIDs, c.Parent = e.workingDir(), claimPIDs(), e.parent
	return c
}

func claimRoutedPlace(ctx context.Context, env Env, s PayloadSession, tool string) (root string, collected, placed bool) {
	if !env.Routing.Usable() || env.lane().Lost != "" {
		return "", false, false
	}
	env.Cwd = cmp.Or(s.Cwd, env.Cwd)
	r, err := env.routeHere(ctx)
	if err != nil {
		return "", false, false
	}
	if r.dest.collected() {
		env.unpark(r)
	}
	placed = env.placeRun(s.ID, s.AgentID, env.repoClaim(r, tool), env.Time())
	return r.Root, r.dest.collected(), placed
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

func (e Env) placeRun(sessionID, agentID string, c claim.Claim, now time.Time) bool {
	if !e.Routing.Usable() || !session.ValidID(sessionID) || e.Routing.WithholdPlacements || e.lane().Lost != "" {
		return false
	}
	c.ProjectID = ""
	c.Profile, c.Account = e.Routing.Profile, e.claimAccount()
	placed := claim.Write(e.StateDir, sessionID, c, now)
	if !placed {
		prev, ok := claim.Read(e.StateDir, sessionID, now)
		placed = ok && prev.CoversPlace(c)
	}
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) {
		claim.Write(e.StateDir, agentID, c, now)
	}
	return placed
}
