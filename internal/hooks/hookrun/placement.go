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

// StartRun records a new clean run for sessionID at session start/resume (never subagent/compact).
func (e Env) StartRun(sessionID string) {
	if e.Routing == nil || !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	if run, err := runs.Start(e.StateDir, sessionID, e.Time()); err != nil {
		e.Logf("start run: %v", err)
	} else {
		e.Routing.setRun(sessionID, run, e.Time())
	}
}

func (e Env) placeFor(r *Repo, ev spool.Event) {
	if r == nil || r.dest == nil || ev.SessionID == "" || e.lane().Lost != "" {
		return
	}
	tool, _ := ev.Attrs[semconv.GenAIMainAgentNameKey].(string)
	agent, _ := ev.Attrs[semconv.TermaAgentIDKey].(string)
	c := r.claim(tool)
	c.Cwd, c.PIDs, c.Parent = e.workingDir(), claimPIDs(), e.parent
	e.judge(ev.SessionID, agent, c, []routing.Location{r.dest.loc}, true, runs.Why{})
	if e.OnClaim != nil && r.dest.collected() {
		e.OnClaim(r.Root)
	}
}

func (e Env) place(sessionID, agentID string, c claim.Claim, loc routing.Location, touched []routing.Location) {
	e.judge(sessionID, agentID, c, append([]routing.Location{loc}, touched...), true, runs.Why{})
}

func (e Env) judge(sessionID, agentID string, c claim.Claim, locs []routing.Location, placed bool, unknown runs.Why) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	now := e.Time()
	cur := e.currentRun(sessionID)
	e.keepRun(sessionID, now)
	mixed := e.recordRun(sessionID, cur, locs, unknown, now)
	if !placed && !mixed {
		return
	}
	c.ProjectID, c.Run = "", cur.id
	e.placeRun(sessionID, agentID, c, mixed, now)
}

func (e Env) recordRun(sessionID string, cur ledgerRun, locs []routing.Location, unknown runs.Why, now time.Time) bool {
	seen := newLocations(cur.locations, locs)
	full := len(cur.locations)+len(seen) > runs.MaxLocations
	prev, havePrev := claim.Read(e.StateDir, sessionID, now)
	mixed, recorded := !cur.mixedAt.IsZero(), true
	var since time.Time
	var why runs.Why
	if !mixed {
		if havePrev && prev.Kind == claim.KindMixed && prev.Run == cur.id {
			since = now
			if n := len(prev.Placements); n > 0 && !prev.Placements[n-1].Since.IsZero() {
				since = prev.Placements[n-1].Since
			}
			mixed, why = true, runs.Why{Reason: runs.Aside}
		} else {
			var crossed bool
			why, crossed = e.Routing.crossing(prev, havePrev, cur.id, locs, cur.locations, now)
			if !crossed && full {
				why = runs.Why{Reason: runs.LocationBound}
			} else if !crossed {
				why = unknown
			}
			if why.Reason != "" {
				mixed, since = true, now
			}
		}
	}
	if !since.IsZero() {
		recorded = e.markMixed(sessionID, cur.id, since, why)
	}
	if full {
		seen = nil
	}
	var merged []string
	var err error
	switch {
	case len(seen) > 0 && recorded:
		merged, err = runs.Touch(e.StateDir, sessionID, cur.id, seen)
	case len(seen) > 0 || cur.aside:
		merged, err = runs.TryTouch(e.StateDir, sessionID, cur.id, seen)
	default:
		return mixed
	}
	if err != nil {
		e.Logf("record run locations: %v", err)
		return mixed
	}
	if len(seen) > 0 {
		e.Routing.updateRun(sessionID, func(lr *ledgerRun) {
			if lr.id == cur.id {
				lr.locations = merged
			}
		})
	}
	if !mixed && len(merged) > len(cur.locations)+len(seen) {
		var w runs.Why
		crossed := len(merged) > runs.MaxLocations
		if crossed {
			w = runs.Why{Reason: runs.LocationBound}
		} else {
			w, crossed = e.Routing.crossing(prev, havePrev, cur.id, locs, merged, now)
		}
		if crossed {
			mixed = true
			e.markMixed(sessionID, cur.id, now, w)
		}
	}
	return mixed
}

func (e Env) placeRun(sessionID, agentID string, c claim.Claim, mixed bool, now time.Time) {
	if e.Routing.WithholdPlacements {
		return
	}
	if mixed {
		c = claim.Claim{Kind: claim.KindMixed, Run: c.Run, Tool: c.Tool, PIDs: c.PIDs, Parent: c.Parent}
	} else if e.lane().Lost != "" {
		return
	}
	c.Profile, c.Account = e.Routing.Profile, e.claimAccount()
	claim.Write(e.StateDir, sessionID, c, now)
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) && claim.Write(e.StateDir, agentID, c, now) {
		if err := runs.Alias(e.StateDir, sessionID, agentID); err != nil {
			e.Logf("alias subagent: %v", err)
		} else {
			e.Routing.updateRun(sessionID, func(lr *ledgerRun) {
				if !slices.Contains(lr.aliases, agentID) {
					lr.aliases = append(slices.Clip(lr.aliases), agentID)
				}
			})
		}
	}
	if !mixed {
		return
	}
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

func (e Env) markMixed(sessionID, run string, at time.Time, why runs.Why) bool {
	set := func() {
		e.Routing.updateRun(sessionID, func(lr *ledgerRun) {
			if lr.id == run && (lr.mixedAt.IsZero() || at.Before(lr.mixedAt)) {
				lr.mixedAt = at
			}
		})
	}
	if err := runs.MarkMixed(e.StateDir, sessionID, at, why); err == nil {
		set()
		return true
	} else {
		e.Logf("mark run mixed (%s %s): %v", why.Reason, why.Where, err)
	}
	if err := runs.MarkAside(e.StateDir, sessionID, run, at); err != nil {
		e.Logf("mark run mixed aside: %v", err)
		return false
	}
	set()
	return false
}

func (rt *Routing) crossing(prev claim.Claim, havePrev bool, run string, locs []routing.Location, been []string, now time.Time) (runs.Why, bool) {
	all := slices.Clone(locs)
	if havePrev {
		placements := prev.Placements
		if len(placements) == 0 {
			placements = []claim.Placement{{Kind: prev.Kind, Run: prev.Run, Repository: prev.Repository}}
		}
		for _, p := range placements {
			if p.Run == run && p.Kind == claim.KindRepository {
				all = append(all, routing.Location{Kind: routing.Repository, Repository: p.Repository.Origin})
			} else if p.Run == run && p.Kind == claim.KindOutsideGit {
				all = append(all, routing.Location{Kind: routing.OutsideGit})
			}
		}
	}
	for _, l := range been {
		if l == ledgerOutsideGit {
			all = append(all, routing.Location{Kind: routing.OutsideGit})
		} else {
			all = append(all, routing.Location{Kind: routing.Repository, Repository: l})
		}
	}
	type org struct {
		id, repo    string
		pending, ok bool
	}
	classes := make([]org, len(all))
	for i, l := range all {
		id, pending, ok := rt.router.OrganizationOf(l, now)
		classes[i] = org{id: id, pending: pending, ok: ok}
		if l.Kind == routing.Repository {
			classes[i].repo = routing.Normalize(l.Repository)
		}
	}
	for i, a := range classes {
		for j := i + 1; j < len(classes); j++ {
			b := classes[j]
			if !a.ok || !b.ok || ((a.pending || b.pending) && a.repo != "" && a.repo == b.repo) || (!a.pending && !b.pending && a.id == b.id) {
				continue
			}
			at := j
			if j >= len(locs) && i < len(locs) {
				at = i
			}
			where := string(all[at].Kind)
			switch all[at].Kind {
			case routing.Repository:
				where = routing.Normalize(all[at].Repository)
			case routing.OutsideGit:
				where = "outside git"
			}
			return runs.Why{Reason: runs.Crossed, Where: where}, true
		}
	}
	return runs.Why{}, false
}

const ledgerOutsideGit = "outside-git"

func newLocations(been []string, locs []routing.Location) []string {
	var out []string
	for _, loc := range locs {
		l := ""
		if loc.Kind == routing.OutsideGit {
			l = ledgerOutsideGit
		} else if loc.Kind == routing.Repository && routing.Owner(loc.Repository) != "" {
			l = routing.Normalize(loc.Repository)
		}
		if l != "" && !slices.Contains(been, l) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}
