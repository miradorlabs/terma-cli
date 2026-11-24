package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const turnStateDir = "turns"

type turnStamp struct {
	Run string    `json:"run,omitempty"`
	At  time.Time `json:"at"`
}

// StartTurn records when a turn began so post-tool reach hooks backdate Mixed cutoffs before the tool ran.
func (e Env) StartTurn(sessionID string) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) || e.StateDir == "" {
		return
	}
	p := filepath.Join(e.StateDir, turnStateDir, sessionID+".json")
	if b, err := json.Marshal(turnStamp{Run: e.runOf(sessionID), At: e.Time().UTC()}); err == nil && os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		_ = WriteState(p, b)
	}
}

// TurnSince returns the current turn's start time (or earliest placement/active time of the run), <= e.Time().
func (e Env) TurnSince(sessionID string) time.Time {
	now := e.Time()
	if !e.Routing.Usable() || !session.ValidID(sessionID) {
		return now
	}
	cur := e.currentRun(sessionID)
	var s turnStamp
	if b, err := os.ReadFile(filepath.Join(e.StateDir, turnStateDir, sessionID+".json")); err == nil && json.Unmarshal(b, &s) == nil && s.Run == cur.id && !s.At.IsZero() && !s.At.After(now) {
		return s.At
	}
	best := cur.active
	if prev, ok := claim.Read(e.StateDir, sessionID, now); ok {
		for _, pl := range prev.Placements {
			if pl.Run == cur.id && !pl.Since.IsZero() && (best.IsZero() || pl.Since.Before(best)) {
				best = pl.Since
			}
		}
	}
	if !best.IsZero() && !best.After(now) {
		return best
	}
	return now
}
func (e Env) mixedCutoff(now time.Time) time.Time {
	if !e.MixedSince.IsZero() && e.MixedSince.Before(now) {
		return e.MixedSince
	}
	return now
}

// StartRun records a new clean run for sessionID at session start/resume (never subagent/compact).
func (e Env) StartRun(sessionID string) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	if run, err := runs.Start(e.StateDir, sessionID, e.Time()); err != nil {
		e.Logf("start run: %v", err)
	} else {
		e.Routing.mu.Lock()
		e.Routing.runs[sessionID] = &ledgerRun{id: run, active: e.Time()}
		e.Routing.mu.Unlock()
	}
}
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
	env.judge(s.ID, s.AgentID, env.repoClaim(r, tool), []routing.Location{r.dest.loc}, true, runs.Why{})
	return r.dest.collected()
}
func (e Env) placeFor(r *Repo, ev spool.Event) {
	if r == nil || r.dest == nil || ev.SessionID == "" || e.lane().Lost != "" {
		return
	}
	tool, _ := ev.Attrs[semconv.GenAIMainAgentNameKey].(string)
	agent, _ := ev.Attrs[semconv.TermaAgentIDKey].(string)
	e.judge(ev.SessionID, agent, e.repoClaim(r, tool), []routing.Location{r.dest.loc}, true, runs.Why{})
	if e.OnClaim != nil && r.dest.collected() {
		e.OnClaim(r.Root)
	}
}
func (e Env) judge(sessionID, agentID string, c claim.Claim, locs []routing.Location, placed bool, unknown runs.Why) {
	if !e.Routing.Usable() || !session.ValidID(sessionID) {
		return
	}
	now, cur := e.Time(), e.currentRun(sessionID)
	e.keepRun(sessionID, now)
	if mixed := e.recordRun(sessionID, cur, locs, unknown, now); placed || mixed {
		c.ProjectID, c.Run = "", cur.id
		e.placeRun(sessionID, agentID, c, mixed, now)
	}
}
func (e Env) recordRun(sessionID string, cur ledgerRun, locs []routing.Location, unknown runs.Why, now time.Time) bool {
	seen := newLocations(cur.locations, locs)
	full := len(cur.locations)+len(seen) > runs.MaxLocations
	prev, havePrev := claim.Read(e.StateDir, sessionID, now)
	mixed, recorded := !cur.mixedAt.IsZero(), true
	if !mixed {
		var since time.Time
		var why runs.Why
		if havePrev && prev.Kind == claim.KindMixed && prev.Run == cur.id {
			mixed, since, why = true, now, runs.Why{Reason: runs.Aside}
			if n := len(prev.Placements); n > 0 && !prev.Placements[n-1].Since.IsZero() {
				since = prev.Placements[n-1].Since
			}
		} else if w, crossed := e.Routing.crossing(prev, havePrev, cur.id, locs, cur.locations, now); crossed {
			mixed, since, why = true, e.mixedCutoff(now), w
		} else if full {
			mixed, since, why = true, e.mixedCutoff(now), runs.Why{Reason: runs.LocationBound}
		} else if unknown.Reason != "" {
			mixed, since, why = true, e.mixedCutoff(now), unknown
		}
		if !since.IsZero() {
			recorded = e.markMixed(sessionID, cur.id, since, why)
		}
	}
	if full {
		seen = nil
	}
	if len(seen) == 0 && !cur.aside {
		return mixed
	}
	touch := runs.TryTouch
	if len(seen) > 0 && recorded {
		touch = runs.Touch
	}
	merged, err := touch(e.StateDir, sessionID, cur.id, seen)
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
		w, crossed := runs.Why{Reason: runs.LocationBound}, len(merged) > runs.MaxLocations
		if !crossed {
			w, crossed = e.Routing.crossing(prev, havePrev, cur.id, locs, merged, now)
		}
		if crossed {
			mixed = true
			e.markMixed(sessionID, cur.id, e.mixedCutoff(now), w)
		}
	}
	return mixed
}
func (e Env) placeRun(sessionID, agentID string, c claim.Claim, mixed bool, now time.Time) {
	if e.Routing.WithholdPlacements {
		return
	}
	at := now
	if mixed {
		c, at = claim.Claim{Kind: claim.KindMixed, Run: c.Run, Tool: c.Tool, PIDs: c.PIDs, Parent: c.Parent}, e.mixedCutoff(now)
	} else if e.lane().Lost != "" {
		return
	}
	c.Profile, c.Account = e.Routing.Profile, e.claimAccount()
	claim.Write(e.StateDir, sessionID, c, at)
	if agentID != "" && agentID != sessionID && session.ValidID(agentID) && claim.Write(e.StateDir, agentID, c, at) {
		if err := runs.Alias(e.StateDir, sessionID, agentID); err != nil {
			e.Logf("alias subagent: %v", err)
		}
	}
	if !mixed {
		return
	}
	if aliases, err := runs.Aliases(e.StateDir, sessionID); err != nil {
		e.Logf("subagents of a mixed run: %v", err)
	} else {
		for _, alias := range aliases {
			if alias != agentID {
				claim.Write(e.StateDir, alias, c, at)
			}
		}
	}
}
func (e Env) markMixed(sessionID, run string, at time.Time, why runs.Why) bool {
	ok := true
	if err := runs.MarkMixed(e.StateDir, sessionID, at, why); err != nil {
		e.Logf("mark run mixed (%s %s): %v", why.Reason, why.Where, err)
		if ok = false; runs.MarkAside(e.StateDir, sessionID, run, at) != nil {
			e.Logf("mark run mixed aside: failed")
			return false
		}
	}
	e.Routing.updateRun(sessionID, func(lr *ledgerRun) {
		if lr.id == run && (lr.mixedAt.IsZero() || at.Before(lr.mixedAt)) {
			lr.mixedAt = at
		}
	})
	return ok
}
func (rt *Routing) crossing(prev claim.Claim, havePrev bool, run string, locs []routing.Location, been []string, now time.Time) (runs.Why, bool) {
	all := slices.Clone(locs)
	if havePrev {
		pls := prev.Placements
		if len(pls) == 0 {
			pls = []claim.Placement{{Kind: prev.Kind, Run: prev.Run, Repository: prev.Repository}}
		}
		for _, p := range pls {
			if p.Run == run && p.Kind == claim.KindRepository {
				all = append(all, routing.Location{Kind: routing.Repository, Repository: p.Repository.Origin})
			} else if p.Run == run && p.Kind == claim.KindOutsideGit {
				all = append(all, routing.Location{Kind: routing.OutsideGit})
			}
		}
	}
	for _, l := range been {
		loc := routing.Location{Kind: routing.Repository, Repository: l}
		if l == ledgerOutsideGit {
			loc = routing.Location{Kind: routing.OutsideGit}
		}
		all = append(all, loc)
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

func newLocations(been []string, locs []routing.Location) (out []string) {
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
