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
// placement, which names no project: the relay routes the session's telemetry by it. Every
// session gets one, outside git and where Route drops the location too, under its session
// ID and the subagent ID its telemetry may use instead. A hook that sees the session's run
// cross organizations (Sessions across organizations) marks the run Mixed in the run ledger
// and writes a mixed placement instead, which only a new run replaces. The run ledger also
// keeps where each run has been, so a crossing two hooks see between them, or that comes
// after the session's claim has expired, is seen too.

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
// classified together, before anything filters any of them. The run ledger then records
// loc and touched among the run's locations, for the hooks after this one.
func (e Env) place(sessionID, agentID string, c claim.Claim, loc routing.Location, touched []routing.Location) {
	rt := e.Routing
	if !rt.Usable() || !session.ValidID(sessionID) {
		return
	}
	now := e.Time()
	run, been := e.currentRun(sessionID)
	c.ProjectID, c.Run = "", run
	seen := newLocations(been, append([]routing.Location{loc}, touched...))
	// Past the bound, the ledger no longer keeps everywhere the run has been, so it cannot
	// rule a crossing out: the run turns Mixed rather than miss one.
	full := len(been)+len(seen) > maxRunLocations
	prev, havePrev := claim.Read(e.StateDir, sessionID, now)
	mixed := havePrev && prev.Kind == claim.KindMixed && prev.Run == c.Run
	if !mixed && (full || rt.crossed(prev, havePrev, c.Run, loc, touched, been, now)) {
		if err := runs.MarkMixed(e.StateDir, sessionID, now); err != nil {
			e.Logf("mark run mixed: %v", err)
		}
		mixed = true
	}
	if len(seen) > 0 && !full {
		merged, err := runs.Touch(e.StateDir, sessionID, run, seen)
		if err != nil {
			e.Logf("record run locations: %v", err)
		} else {
			rt.recorded(sessionID, run, merged)
		}
		// Another hook of the run (its subagent's, say) may have recorded locations after
		// this one read the ledger: Touch read them under the lock it wrote under, so of two
		// hooks that each saw half a crossing, the later writer sees it whole.
		if !mixed && len(merged) > len(been)+len(seen) &&
			(len(merged) > maxRunLocations || rt.crossed(prev, havePrev, c.Run, loc, touched, merged, now)) {
			if err := runs.MarkMixed(e.StateDir, sessionID, now); err != nil {
				e.Logf("mark run mixed: %v", err)
			}
			mixed = true
		}
	}
	if mixed {
		c = claim.Claim{Kind: claim.KindMixed, Run: c.Run, Tool: c.Tool, PIDs: c.PIDs}
	}
	claim.Write(e.StateDir, sessionID, c, now)
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) {
		// A subagent whose telemetry names its own ID shares the session's run: it is an
		// alias in the run ledger, so its crossing is the session's and the session's is
		// its own.
		if claim.Write(e.StateDir, agentID, c, now) {
			if err := runs.Alias(e.StateDir, sessionID, agentID); err != nil {
				e.Logf("alias subagent: %v", err)
			}
		}
	}
	if !mixed {
		return
	}
	// Mixed covers every identity the run's telemetry uses: each subagent that shares it,
	// not only the one whose hook saw the crossing.
	aliases, err := runs.Aliases(e.StateDir, sessionID)
	if err != nil {
		e.Logf("subagents of a mixed run: %v", err)
	}
	for _, alias := range aliases {
		if alias != agentID {
			claim.Write(e.StateDir, alias, c, now)
		}
	}
}

// crossed reports whether the run run, whose session's claim is prev and which the run
// ledger records at been, crosses organizations at loc: whether any two of its locations,
// from its placements, the ledger, loc and the checkouts touched, belong to different
// organizations (crosses), each classified now, whenever it was recorded.
func (rt *Routing) crossed(prev claim.Claim, havePrev bool, run string, loc routing.Location, touched []routing.Location, been []string, now time.Time) bool {
	locs := append([]routing.Location{loc}, touched...)
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

// maxRunLocations bounds the locations the run ledger keeps for one run. A run touches few
// checkouts; one that has been to more turns Mixed (place).
const maxRunLocations = 64

// ledgerOutsideGit is how the run ledger records a location outside git. A repository ID
// always has a host and a path, so it is never one.
const ledgerOutsideGit = "outside-git"

// ledgerLocation is how the run ledger records loc: its normalised repository, or
// ledgerOutsideGit; "" for one crossed cannot classify, which belongs to no organization
// whenever it is checked.
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

// newLocations are the locations among locs, as the run ledger records them, that been, the
// run's, does not have yet: each once.
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
