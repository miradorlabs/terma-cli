package history

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fileSnapshot{}, false
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

type dryRunRouteEst struct {
	team    routing.Team
	records int
	bytes   int64
}

// measureDryRunSession streams a second bounded-memory pass over f during --dry-run to
// count filtered records and estimate protobuf bytes per route without retaining segments.
func measureDryRunSession(
	ctx context.Context,
	opts Options,
	src agents.HistorySource,
	f agents.HistoryFile,
	key string,
	resolver *cwdResolver,
	existingStates map[string]RouteState,
	resumableByRoute map[string]map[string]bool,
	reachMixedAt time.Time,
	hasReachMixed bool,
	userIDFor func(routing.Team, string) string,
	sum *Summary,
) (map[string]dryRunRouteEst, error) {
	snap, lookup := newRunSnapLookup(opts.StateDir, f.SessionID)
	byRoute := map[string]dryRunRouteEst{}
	err := src.ReadHistory(ctx, f, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seg.RecordCount == 0 {
			return nil
		}
		target, ok, _ := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
		if !ok {
			return nil
		}
		eid := target.Team.EnrollmentID
		if f.OutsideSince && !resumableByRoute[eid][key] {
			return nil
		}
		st, hasState := existingStates[eid]
		_, prevFailed := st.IsFailed(key)
		if hasState && (st.IsDone(key) || prevFailed) {
			cur := byRoute[eid]
			cur.team = target.Team
			cur.records += seg.RecordCount
			byRoute[eid] = cur
			return nil
		}
		var resumeMax time.Time
		if prev, ok := st.InProgress[key]; ok && !prev.SourceModTime.IsZero() && f.ModTime.After(prev.SourceModTime) {
			resumeMax = prev.SourceModTime
		}
		prepared, n, mixedDropped := prepareSegmentFiltered(seg, opts.Filter, target.Team.Policy, buildStampOptions(opts, target, userIDFor), snap, lookup, reachMixedAt, hasReachMixed, resumeMax)
		if n == 0 || prepared == nil {
			if mixedDropped > 0 {
				sum.addSkippedSegment(SkipMixed, 1)
			} else {
				sum.addSkippedSegment(SkipEmptyAfterFilter, 1)
			}
			return nil
		}
		cur := byRoute[eid]
		cur.team = target.Team
		cur.records += n
		cur.bytes += int64(proto.Size(prepared))
		byRoute[eid] = cur
		return nil
	})
	return byRoute, err
}

// classifyReachedPath classifies a reached path without spawning git subprocesses.
// Uses gitx.LocateFS so deleted subdirectories inside an existing checkout still resolve
// to their enclosing repository, while missing paths outside baseRoot and outside
// nobody's directories fail closed (unknown=true) even when no Personal team is selected.
func classifyReachedPath(cand, baseRoot string) reachedClass {
	info, statErr := os.Stat(cand)
	if statErr == nil {
		dir := cand
		if !info.IsDir() {
			dir = filepath.Dir(cand)
		}
		if _, gitDir, ok := gitx.LocateFS(dir); ok && gitDir != "" {
			if origin, _ := gitx.OriginFS(gitDir); routing.Owner(origin) != "" {
				return reachedClass{loc: routing.Location{Kind: routing.Repository, Repository: origin}}
			}
		}
		if hookrun.IsNobodyDir(dir) {
			return reachedClass{nobody: true}
		}
		return reachedClass{loc: routing.Location{Kind: routing.OutsideGit}}
	}
	if root, gitDir, ok := gitx.LocateFS(cand); ok && gitDir != "" {
		if baseRoot != "" && sameCleanPath(root, baseRoot) {
			return reachedClass{nobody: true}
		}
		if origin, _ := gitx.OriginFS(gitDir); routing.Owner(origin) != "" {
			return reachedClass{loc: routing.Location{Kind: routing.Repository, Repository: origin}}
		}
	}
	if baseRoot != "" && isSubpathOf(cand, baseRoot) {
		return reachedClass{nobody: true}
	}
	if hookrun.IsNobodyDir(cand) || hookrun.IsNobodyDir(filepath.Dir(cand)) {
		return reachedClass{nobody: true}
	}
	return reachedClass{unknown: true}
}

func sameCleanPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ra, ea := filepath.EvalSymlinks(ca)
	rb, eb := filepath.EvalSymlinks(cb)
	return ea == nil && eb == nil && ra == rb
}

func isSubpathOf(path, base string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
