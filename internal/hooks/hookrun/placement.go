package hookrun

import (
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// On a migrated machine a hook tells the relay where a session runs with a location-only
// placement, which names no project: the relay routes the session's telemetry by it. Every
// session gets one, outside git and where Route drops the location too, under its session
// ID and the subagent ID its telemetry may use instead. A hook that sees the session's run
// cross organizations (Sessions across organizations) marks the run Mixed in the run ledger
// and writes a mixed placement instead, which only a new run replaces.

// StartRun records, on a migrated machine, that sessionID begins a new run now: a session
// start or a resume, never a subagent starting nor a session merely compacted. Its
// placements and spooled events name the run from now on, and the new run is not Mixed,
// whatever the last one was. Elsewhere it does nothing.
func (e Env) StartRun(sessionID string) {
	if e.Routing == nil || !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	run, err := runs.Start(e.StateDir, sessionID, e.Time())
	if err != nil {
		e.Logf("start run: %v", err)
		return
	}
	e.Routing.setRun(sessionID, run)
}

// placeFor is claimForRelay on a migrated machine: the event's session's location-only
// placement at r, under its ID and its subagent's, or its run marked Mixed.
func (e Env) placeFor(r *Repo, ev spool.Event) {
	if r == nil || r.dest == nil || ev.SessionID == "" {
		return
	}
	tool, _ := ev.Attrs[semconv.GenAIMainAgentNameKey].(string)
	agent, _ := ev.Attrs[semconv.TermaAgentIDKey].(string)
	c := r.claim(tool)
	c.Cwd, c.PIDs = e.workingDir(), claimPIDs()
	e.place(ev.SessionID, agent, c, r.dest.loc, nil)
	if e.OnClaim != nil && r.dest.collected() {
		e.OnClaim(r.Root)
	}
}

// place records that sessionID's run is at loc now, with c, the placement's checkout and
// processes: c's location-only placement under the session's ID and agentID's, unless the
// run has crossed organizations, when it is marked Mixed instead. touched are the other
// checkouts the hook saw the session touch: loc, the run's earlier locations and they are
// classified together, before anything filters any of them.
func (e Env) place(sessionID, agentID string, c claim.Claim, loc routing.Location, touched []routing.Location) {
	rt := e.Routing
	if !rt.Usable() || !session.ValidID(sessionID) {
		return
	}
	now := e.Time()
	c.ProjectID, c.Run = "", e.runOf(sessionID)
	prev, havePrev := claim.Read(e.StateDir, sessionID, now)
	mixed := havePrev && prev.Kind == claim.KindMixed && prev.Run == c.Run
	if !mixed && rt.crossed(prev, havePrev, c.Run, loc, touched, now) {
		if err := runs.MarkMixed(e.StateDir, sessionID, now); err != nil {
			e.Logf("mark run mixed: %v", err)
		}
		mixed = true
	}
	if mixed {
		c = claim.Claim{Kind: claim.KindMixed, Run: c.Run, Tool: c.Tool, PIDs: c.PIDs}
	}
	claim.Write(e.StateDir, sessionID, c, now)
	if agentID == "" || agentID == sessionID || !session.ValidID(agentID) {
		return
	}
	// A subagent whose telemetry names its own ID shares the session's run: it is an alias
	// in the run ledger, so its crossing is the session's and the session's is its own.
	if claim.Write(e.StateDir, agentID, c, now) {
		if err := runs.Alias(e.StateDir, sessionID, agentID); err != nil {
			e.Logf("alias subagent: %v", err)
		}
	}
}

// crossed reports whether the run run, whose session's claim is prev, crosses
// organizations at loc: whether any two of its locations, from its placements, loc and the
// checkouts touched, belong to different organizations (crosses).
func (rt *Routing) crossed(prev claim.Claim, havePrev bool, run string, loc routing.Location, touched []routing.Location, now time.Time) bool {
	locs := append([]routing.Location{loc}, touched...)
	if havePrev {
		locs = append(locs, runLocations(prev, run)...)
	}
	classes := make([]organization, 0, len(locs))
	for _, l := range locs {
		classes = append(classes, rt.organizationOf(l, now))
	}
	for i, a := range classes {
		for _, b := range classes[i+1:] {
			if crosses(a, b) {
				return true
			}
		}
	}
	return false
}

// runLocations are the locations of c's placements that belong to run. A legacy placement
// says nothing of where it ran, and a mixed one has crossed already.
func runLocations(c claim.Claim, run string) []routing.Location {
	placements := c.Placements
	if len(placements) == 0 {
		placements = []claim.Placement{{Kind: c.Kind, Run: c.Run, Repository: c.Repository}}
	}
	var out []routing.Location
	for _, p := range placements {
		switch {
		case p.Run != run:
		case p.Kind == claim.KindRepository:
			out = append(out, routing.Location{Kind: routing.Repository, Repository: p.Repository.Origin})
		case p.Kind == claim.KindOutsideGit:
			out = append(out, routing.Location{Kind: routing.OutsideGit})
		}
	}
	return out
}

// organization is the organization a location belongs to, for telling a crossing.
type organization struct {
	id string
	// pending is a repository whose binding is still to be decided: it belongs to no
	// organization a run can share.
	pending bool
	// repo is the normalised repository, which tells two locations in one pending
	// repository, such as two of its worktrees, apart from two pending ones.
	repo string
	ok   bool
}

func (rt *Routing) organizationOf(loc routing.Location, now time.Time) organization {
	id, pending, ok := rt.router.OrganizationOf(loc, now)
	o := organization{id: id, pending: pending, ok: ok}
	if loc.Kind == routing.Repository {
		o.repo = routing.Normalize(loc.Repository)
	}
	return o
}

// crosses reports whether a and b belong to different organizations. A location nothing
// collects and nothing binds belongs to none and never crosses; a pending one crosses
// anything but its own repository.
func crosses(a, b organization) bool {
	switch {
	case !a.ok || !b.ok:
		return false
	case a.pending || b.pending:
		return a.repo == "" || a.repo != b.repo
	}
	return a.id != b.id
}
