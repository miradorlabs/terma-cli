package claim

import "time"

// At returns the claim placement covering pid at time at (empty ProjectID for Mark, false
// if uncovered). Legacy claims pick the latest covering placement <= at (or earliest if
// older, latest if untimed). Kinded claims refuse records older than all kept placements
// once Evicted, and refuse untimed records unless Evicted is false and every covering
// placement shares the same Kind, Repository, Profile, Account, and Run.
func (c Claim) At(pid int, at time.Time) (Claim, bool) {
	all := c.placements()
	var covering []*Placement
	kinds := false
	for i := range all {
		kinds = kinds || all[i].Kind != ""
		if covers(all[i].PIDs, pid) {
			covering = append(covering, &all[i])
		}
	}
	if len(covering) == 0 {
		return Claim{}, false
	}
	best := inForce(covering, at)
	if kinds {
		best = locate(covering, at, c.Evicted)
	}
	if best == nil {
		return Claim{}, false
	}
	return Claim{ProjectID: best.ProjectID, Kind: best.Kind, Run: best.Run, Profile: best.Profile, Account: best.Account, Tool: best.Tool, Repo: best.Repo, Worktree: best.Worktree,
		Repository: best.Repository, Root: best.Root, Cwd: best.Cwd, Remote: best.Remote, Branch: best.Branch, PIDs: best.PIDs, ClaimedAt: c.ClaimedAt, Parent: c.Parent, Placements: c.Placements, Evicted: c.Evicted}, true
}

// inForce is the placement in force at at among covering, oldest first, as a legacy claim
// reads it: the latest to start by at, else the earliest; the latest when at is zero.
func inForce(covering []*Placement, at time.Time) *Placement {
	var best *Placement
	for _, p := range covering {
		if best == nil || at.IsZero() || !p.Since.After(at) {
			best = p
		}
	}
	return best
}

// locate is inForce for a claim with kinds, nil where the record's placement is not known.
func locate(covering []*Placement, at time.Time, evicted bool) *Placement {
	if at.IsZero() {
		if evicted {
			return nil
		}
		for _, p := range covering {
			if p.Kind != covering[0].Kind || p.Repository != covering[0].Repository || p.Profile != covering[0].Profile || p.Account != covering[0].Account || p.Run != covering[0].Run {
				return nil
			}
		}
		return covering[len(covering)-1]
	}
	var best *Placement
	for _, p := range covering {
		if !p.Since.After(at) {
			best = p
		}
	}
	if best == nil && !evicted {
		best = covering[0]
	}
	return best
}
