package claim

import "time"

// At is the claim as it applies to a record pid sent at time at: the placement covering
// pid that was in force when the record was made, whose ProjectID it carries, empty for
// one Mark wrote. False when no placement is known to cover the record.
//
// Among the placements covering pid, the latest to start by at is in force. A record
// older than all of them is the first export racing the session's first hook, and takes
// the earliest. A legacy claim, whose placements have no kind, is read that way alone,
// and a record without a time takes the latest placement.
//
// A claim with kinds follows the plan's "Where a record ran" as well:
//   - once a placement has been evicted, a record older than every covering placement
//     kept is not covered, since it may be the evicted one's;
//   - a record without a time takes the latest covering placement only if none has been
//     evicted and every covering placement has the same Kind and Repository, so the
//     time could not have chosen another location; otherwise it is not covered.
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
	var best *Placement
	if kinds {
		best = locate(covering, at, c.Evicted)
	} else {
		best = inForce(covering, at)
	}
	if best == nil {
		return Claim{}, false
	}
	return Claim{ProjectID: best.ProjectID, Kind: best.Kind, Run: best.Run, Profile: best.Profile, Tool: best.Tool, Repo: best.Repo, Worktree: best.Worktree,
		Repository: best.Repository, Root: best.Root, Cwd: best.Cwd, Remote: best.Remote, Branch: best.Branch,
		PIDs: best.PIDs, ClaimedAt: c.ClaimedAt, Placements: c.Placements, Evicted: c.Evicted}, true
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
			if p.Kind != covering[0].Kind || p.Repository != covering[0].Repository {
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
