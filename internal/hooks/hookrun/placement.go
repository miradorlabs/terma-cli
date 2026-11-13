package hookrun

import (
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// On a migrated machine a hook tells the relay where a session runs with a location-only
// placement, naming no project, under the session's ID and its subagent's. The relay routes
// the session's telemetry by it. A run that crosses organizations is marked Mixed in the
// run ledger and gets a mixed placement, which only a new run replaces. The ledger keeps
// where each run has been, so a crossing split across hooks, or past the claim, is seen.

// StartRun records, on a migrated machine, that sessionID begins a new run now: at a
// session start or resume, not a subagent start or a compaction. What the session records
// names the new run from now on, and it is not Mixed. Elsewhere it does nothing.
func (e Env) StartRun(sessionID string) {
	if e.Routing == nil || !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	run, err := runs.Start(e.StateDir, sessionID, e.Time())
	if err != nil {
		e.Logf("start run: %v", err)
		return
	}
	e.Routing.setRun(sessionID, run, e.Time())
}

// placeFor is claimForRelay on a migrated machine: it places the event's session at r.
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

// place records that sessionID's run is at loc now, with c's checkout and processes, and
// touched touched: c's placement under the session's ID and agentID's, or a mixed one.
func (e Env) place(sessionID, agentID string, c claim.Claim, loc routing.Location, touched []routing.Location) {
	e.judge(sessionID, agentID, c, append([]routing.Location{loc}, touched...), true)
}

// judge classifies locs (where the run is, and what it touched) with the run's earlier
// locations (decide), records the result in the run ledger (recordRun), and writes the
// placement: c at locs[0] when placed, else only a mixed one (placeRun). The ledger, with
// its sidecar, decides Mixed, since delivery and the relay read only it.
func (e Env) judge(sessionID, agentID string, c claim.Claim, locs []routing.Location, placed bool) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	now := e.Time()
	cur := e.currentRun(sessionID)
	e.keepRun(sessionID, now)
	mixed := e.recordRun(sessionID, cur, e.decide(sessionID, cur, locs, now), now)
	if !placed && !mixed {
		return
	}
	c.ProjectID, c.Run = "", cur.id
	e.placeRun(sessionID, agentID, c, mixed, now)
}

// verdict is what decide made of a hook's locations, for recordRun to write.
type verdict struct {
	// seen are the locations new to the run, as the ledger records them; full is set past
	// maxRunLocations, where the ledger records no more.
	seen []string
	full bool
	// mixed: the run is Mixed. since, when set, is when it turned Mixed, which the ledger
	// does not record yet.
	mixed bool
	since time.Time
	// crossedWith classifies the hook's locations again with been, all the run's locations
	// the ledger returns.
	crossedWith func(been []string) bool
}

// decide classifies locs with where cur, the run, has been and its placement. It writes
// nothing. A mixed placement the ledger does not back is a crossing both of whose writes
// failed (recordRun): it is recorded again, from when the placement turned Mixed, while the
// placement lasts.
func (e Env) decide(sessionID string, cur ledgerRun, locs []routing.Location, now time.Time) verdict {
	rt := e.Routing
	v := verdict{seen: newLocations(cur.locations, locs)}
	// Past the bound the ledger cannot rule a crossing out, so the run turns Mixed.
	v.full = len(cur.locations)+len(v.seen) > maxRunLocations
	prev, havePrev := claim.Read(e.StateDir, sessionID, now)
	v.crossedWith = func(been []string) bool {
		return len(been) > maxRunLocations || rt.crossed(prev, havePrev, cur.id, locs, been, now)
	}
	switch {
	case !cur.mixedAt.IsZero():
		v.mixed = true
	case havePrev && prev.Kind == claim.KindMixed && prev.Run == cur.id:
		v.mixed, v.since = true, mixedSince(prev, now)
	case v.full || rt.crossed(prev, havePrev, cur.id, locs, cur.locations, now):
		v.mixed, v.since = true, now
	}
	return v
}

// recordRun writes v to the run ledger, a crossing it found and the locations it saw, and
// reports whether the run is Mixed. A crossing the ledger refuses (its lock held past its
// wait, or a failed write) goes to its sidecar, which never expires (markMixed), and the
// locations are recorded only if the lock is free now: a second wait would most likely
// fail too, and a Mixed run's locations decide nothing until a new run starts. The run's
// next hook that can lock the ledger folds the sidecar in.
func (e Env) recordRun(sessionID string, cur ledgerRun, v verdict, now time.Time) bool {
	rt := e.Routing
	mixed, recorded := v.mixed, true
	if !v.since.IsZero() {
		recorded = e.markMixed(sessionID, cur.id, v.since)
	}
	var seen []string
	if !v.full {
		seen = v.seen
	}
	var merged []string
	var err error
	switch {
	case len(seen) > 0 && recorded:
		merged, err = runs.Touch(e.StateDir, sessionID, cur.id, seen)
	case len(seen) > 0 || cur.aside:
		// Waiting for nothing: this hook already waited on a refused lock, or the lock
		// only folds the sidecar in.
		merged, err = runs.TryTouch(e.StateDir, sessionID, cur.id, seen)
	default:
		return mixed
	}
	switch {
	case err != nil:
		e.Logf("record run locations: %v", err)
		return mixed
	case len(seen) > 0:
		rt.recorded(sessionID, cur.id, merged)
	}
	// Touch returns what other hooks recorded since this one read the ledger: of two hooks
	// that each saw half a crossing, the later sees it whole.
	if !mixed && len(merged) > len(cur.locations)+len(seen) && v.crossedWith(merged) {
		mixed = true
		e.markMixed(sessionID, cur.id, now)
	}
	return mixed
}

// placeRun writes c, the run's placement, under sessionID and agentID's, or a mixed one,
// which also replaces the placement of every subagent sharing the run.
func (e Env) placeRun(sessionID, agentID string, c claim.Claim, mixed bool, now time.Time) {
	if mixed {
		c = claim.Claim{Kind: claim.KindMixed, Run: c.Run, Tool: c.Tool, PIDs: c.PIDs}
	}
	claim.Write(e.StateDir, sessionID, c, now)
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) {
		// A subagent whose telemetry names its own ID shares the run, as a ledger alias.
		if claim.Write(e.StateDir, agentID, c, now) {
			if err := runs.Alias(e.StateDir, sessionID, agentID); err != nil {
				e.Logf("alias subagent: %v", err)
			} else {
				e.Routing.aliased(sessionID, agentID)
			}
		}
	}
	if !mixed {
		return
	}
	// Mixed covers every subagent sharing the run, not only the one whose hook saw it.
	aliases, err := e.Routing.aliases(e.StateDir, sessionID)
	if err != nil {
		e.Logf("subagents of a mixed run: %v", err)
	}
	for _, alias := range aliases {
		if alias != agentID {
			claim.Write(e.StateDir, alias, c, now)
		}
	}
}

// markMixed records in the run ledger that sessionID's run run became Mixed at at, and
// reports whether the ledger took it. One that cannot take it now gets it in its sidecar
// (runs.MarkAside), without its lock: every read of the ledger takes that for Mixed, and it
// never expires, as the mixed placement does.
func (e Env) markMixed(sessionID, run string, at time.Time) bool {
	err := runs.MarkMixed(e.StateDir, sessionID, at)
	if err == nil {
		e.Routing.marked(sessionID, run, at)
		return true
	}
	e.Logf("mark run mixed: %v", err)
	if err := runs.MarkAside(e.StateDir, sessionID, run, at); err != nil {
		e.Logf("mark run mixed aside: %v", err)
		return false
	}
	e.Routing.marked(sessionID, run, at)
	return false
}

// mixedSince is when c, a mixed placement, turned Mixed: its latest placement's start,
// which rewrites keep; now when it names none.
func mixedSince(c claim.Claim, now time.Time) time.Time {
	if n := len(c.Placements); n > 0 && !c.Placements[n-1].Since.IsZero() {
		return c.Placements[n-1].Since
	}
	return now
}

// crossed reports whether any two of run's locations (locs, been from the ledger, and
// prev's placements) belong to different organizations, each classified now.
func (rt *Routing) crossed(prev claim.Claim, havePrev bool, run string, locs []routing.Location, been []string, now time.Time) bool {
	locs = slices.Clone(locs)
	if havePrev {
		locs = append(locs, runLocations(prev, run)...)
	}
	for _, l := range been {
		locs = append(locs, fromLedger(l))
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

// maxRunLocations bounds the locations the ledger keeps for a run; past it the run turns
// Mixed (judge).
const maxRunLocations = 64

// ledgerOutsideGit is how the ledger records outside git; no repository ID can equal it.
const ledgerOutsideGit = "outside-git"

// ledgerLocation is how the ledger records loc: its normalised repository, or
// ledgerOutsideGit; "" for one that belongs to no organization.
func ledgerLocation(loc routing.Location) string {
	switch {
	case loc.Kind == routing.OutsideGit:
		return ledgerOutsideGit
	case loc.Kind == routing.Repository && routing.Owner(loc.Repository) != "":
		return routing.Normalize(loc.Repository)
	}
	return ""
}

// fromLedger is the location the run ledger records as s.
func fromLedger(s string) routing.Location {
	if s == ledgerOutsideGit {
		return routing.Location{Kind: routing.OutsideGit}
	}
	return routing.Location{Kind: routing.Repository, Repository: s}
}

// newLocations are locs as the ledger records them, minus those in been, each once.
func newLocations(been []string, locs []routing.Location) []string {
	var out []string
	for _, loc := range locs {
		if l := ledgerLocation(loc); l != "" && !slices.Contains(been, l) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// organization is the organization a location belongs to, for telling a crossing.
type organization struct {
	id string
	// pending is a repository whose binding is undecided: no run can share it.
	pending bool
	// repo tells two worktrees of one pending repository from two pending repositories.
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

// crosses reports whether a and b belong to different organizations. A location no team
// collects or binds never crosses; a pending one crosses all but its own repository.
func crosses(a, b organization) bool {
	switch {
	case !a.ok || !b.ok:
		return false
	case a.pending || b.pending:
		return a.repo == "" || a.repo != b.repo
	}
	return a.id != b.id
}
