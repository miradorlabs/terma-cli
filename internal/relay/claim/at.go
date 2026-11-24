package claim

import (
	"slices"
	"time"
)

// At returns the placement covering pid at time at (empty ProjectID for Mark, false if
// uncovered). Kinded claims refuse records before kept placements once Evicted and refuse
// untimed records unless Evicted is false and covering placements agree on ProjectID/Kind/Repository/Profile/Account/Run.
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
func inForce(covering []*Placement, at time.Time) *Placement {
	var best *Placement
	for _, p := range covering {
		if best == nil || at.IsZero() || !p.Since.After(at) {
			best = p
		}
	}
	return best
}
func locate(covering []*Placement, at time.Time, evicted bool) *Placement {
	if at.IsZero() {
		if evicted || slices.ContainsFunc(covering, func(p *Placement) bool {
			return p.ProjectID != covering[0].ProjectID || p.Kind != covering[0].Kind || p.Repository != covering[0].Repository || p.Profile != covering[0].Profile || p.Account != covering[0].Account || p.Run != covering[0].Run
		}) {
			return nil
		}
		return covering[len(covering)-1]
	}
	if best := inForce(covering, at); !evicted || !best.Since.After(at) {
		return best
	}
	return nil
}
