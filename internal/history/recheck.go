package history

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

type fileSnapshot struct {
	modTime time.Time
	size    int64
	count   int
}

func statHistoryFile(f agents.HistoryFile) (fileSnapshot, bool) {
	if f.Path == "" {
		return fileSnapshot{modTime: f.ModTime}, true
	}
	info, err := os.Lstat(f.Path)
	if err != nil || !info.Mode().IsRegular() {
		return fileSnapshot{}, false
	}
	snap := fileSnapshot{
		modTime: info.ModTime(),
		size:    info.Size(),
		count:   1,
	}
	seen := map[string]bool{filepath.Clean(f.Path): true}
	addFile := func(path string, required bool) bool {
		clean := filepath.Clean(path)
		if seen[clean] {
			return true
		}
		seen[clean] = true
		fi, err := os.Lstat(clean)
		if err != nil || !fi.Mode().IsRegular() {
			return !required
		}
		if fi.ModTime().After(snap.modTime) {
			snap.modTime = fi.ModTime()
		}
		snap.size += fi.Size()
		snap.count++
		return true
	}
	for _, p := range f.ExtraPaths {
		if !addFile(p, true) {
			return fileSnapshot{}, false
		}
	}
	for _, dir := range f.WatchDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			addFile(filepath.Join(dir, ent.Name()), false)
		}
	}
	return snap, true
}

func statSessionFiles(f agents.HistoryFile, descendants []agents.HistoryFile) (fileSnapshot, bool) {
	snap, ok := statHistoryFile(f)
	if !ok {
		return fileSnapshot{}, false
	}
	for _, d := range descendants {
		ds, dok := statHistoryFile(d)
		if !dok {
			return fileSnapshot{}, false
		}
		if ds.modTime.After(snap.modTime) {
			snap.modTime = ds.modTime
		}
		snap.size += ds.size
		snap.count += ds.count
	}
	return snap, true
}

func policyContentMatches(a, b config.Policy) bool {
	ap, at := a.Content()
	bp, bt := b.Content()
	return ap == bp && at == bt &&
		a.CollectsNothing == b.CollectsNothing &&
		a.Unset == b.Unset &&
		a.Mode == b.Mode &&
		a.Wildcards == b.Wildcards &&
		slices.Equal(a.Repositories, b.Repositories)
}

// routingPoliciesMatch reports whether two routing snapshots have identical routing
// semantics (selections, bindings, team availability, and policy rules/content switches),
// ignoring FetchedAt-only Generation ticks from routine background policy refreshes.
func routingPoliciesMatch(a, b routing.State, now time.Time) bool {
	if a.StateID != b.StateID || a.Account != b.Account || a.ConsentGeneration != b.ConsentGeneration {
		return false
	}
	if len(a.Teams) != len(b.Teams) || !maps.Equal(a.Bindings, b.Bindings) {
		return false
	}
	for _, ta := range a.Teams {
		tb, ok := b.Team(ta.EnrollmentID)
		if !ok {
			return false
		}
		if ta.OrganizationID != tb.OrganizationID || ta.TeamID != tb.TeamID ||
			ta.Personal != tb.Personal || ta.AuthURL != tb.AuthURL ||
			ta.StateAt(now) != tb.StateAt(now) || !policyContentMatches(ta.Policy, tb.Policy) {
			return false
		}
	}
	return true
}

func recheckRouteTeam(opts Options, enrollmentID string, now time.Time) (routing.State, routing.Team, string, string, bool) {
	st := opts.State
	if opts.ReloadState != nil {
		fresh, err := opts.ReloadState()
		if err != nil {
			return routing.State{}, routing.Team{}, SkipDeselected, err.Error(), false
		}
		st = fresh
	}
	if st.Account != opts.Account {
		return routing.State{}, routing.Team{}, SkipDeselected, "active sign-in account changed during upload", false
	}
	team, ok := st.Team(enrollmentID)
	if !ok {
		return routing.State{}, routing.Team{}, SkipDeselected, "team is no longer selected", false
	}
	if state := team.StateAt(now); state != routing.Active {
		reason := string(state)
		return routing.State{}, routing.Team{}, SkipTeamBlockedPrefix + reason, "team is currently blocked (" + reason + ")", false
	}
	if team.Policy.CollectsNothing || team.Policy.Unset {
		return routing.State{}, routing.Team{}, SkipNotCollected, "team policy no longer collects telemetry", false
	}
	if opts.AuthURL != "" && team.AuthURL != "" && !sameHost(team.AuthURL, opts.AuthURL) {
		return routing.State{}, routing.Team{}, SkipWrongEnvironment, "team belongs to another environment", false
	}
	if opts.ServerKey {
		if opts.AuthURL != "" && !sameHost(opts.ServerKeyAuthURL, opts.AuthURL) {
			return routing.State{}, routing.Team{}, SkipWrongEnvironment, "server key belongs to another environment", false
		}
		if opts.ServerKeyTeam == "" || team.TeamID != opts.ServerKeyTeam {
			return routing.State{}, routing.Team{}, SkipNotCollected, "team does not match configured server-key team", false
		}
	}
	return st, team, "", "", true
}

func orgEpochSnapshot(teams []routing.Team, orgID, authURL string) map[string]int64 {
	out := map[string]int64{}
	for _, t := range teams {
		if t.OrganizationID == orgID && t.AuthURL == authURL {
			out[t.EnrollmentID] = t.Epoch
		}
	}
	return out
}

func buildStampOptions(opts Options, target RouteTarget, userIDFor func(routing.Team, string) string) relay.StampOptions {
	endpoint := ""
	if opts.EndpointFor != nil {
		endpoint = opts.EndpointFor(target.Team.TeamID)
	}
	userID := ""
	if userIDFor != nil {
		userID = userIDFor(target.Team, endpoint)
	}
	return relay.StampOptions{
		ProjectID: target.Team.TeamID,
		Version:   opts.Version,
		UserID:    userID,
		Checkout:  target.Checkout,
	}
}

func newRunSnapLookup(stateDir, sessionID string) (runs.Snapshot, func(string) runs.Snapshot) {
	mainSnap := runs.Read(stateDir, sessionID)
	subSnaps := map[string]runs.Snapshot{}
	return mainSnap, func(id string) runs.Snapshot {
		if id == sessionID {
			return mainSnap
		}
		if s, ok := subSnaps[id]; ok {
			return s
		}
		s := runs.Read(stateDir, id)
		subSnaps[id] = s
		return s
	}
}

func sortedPlans(m map[string]*routeSessionPlan) []*routeSessionPlan {
	out := make([]*routeSessionPlan, 0, len(m))
	for _, sp := range m {
		out = append(out, sp)
	}
	slices.SortFunc(out, func(a, b *routeSessionPlan) int {
		return cmp.Or(a.file.ModTime.Compare(b.file.ModTime), strings.Compare(a.key, b.key))
	})
	return out
}
