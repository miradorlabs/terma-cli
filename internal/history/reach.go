package history

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

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

type resolvedSegment struct {
	seg        agents.HistorySegment
	target     RouteTarget
	ok         bool
	skipReason string
}

type reachedClass struct {
	loc     routing.Location
	nobody  bool
	unknown bool
}

// sessionReachMixedAt inspects tool reaches across every segment of a session (including
// companion subagents) using the same static expansion, nobodys filter, and
// routing.Router.OrganizationOf check that live reach hooks apply. Returning one earliest
// cutoff for the session ensures a cross-org reach is not reset by a later cwd change
// (such as a subdirectory or worktree switch) or hidden by out-of-order subagent entries.
func sessionReachMixedAt(resolver *cwdResolver, segs []resolvedSegment) (time.Time, bool) {
	if resolver == nil || len(segs) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	found := false
	mark := func(at time.Time) {
		if !found || at.IsZero() || (!earliest.IsZero() && at.Before(earliest)) {
			earliest, found = at, true
		}
	}

	type orgClass struct {
		org, repo   string
		pending, ok bool
	}
	orgOf := func(loc routing.Location) orgClass {
		org, pending, ok := resolver.router.OrganizationOf(loc, resolver.now)
		repo := ""
		if loc.Kind == routing.Repository {
			repo = routing.Normalize(loc.Repository)
		}
		return orgClass{org: org, repo: repo, pending: pending, ok: ok}
	}
	crosses := func(a, b orgClass) bool {
		if !a.ok || !b.ok {
			return false
		}
		if (a.pending || b.pending) && a.repo != "" && a.repo == b.repo {
			return false
		}
		return a.pending || b.pending || a.org != b.org
	}

	// If a companion subagent ran in a different organization than the main session,
	// mark the shared session Mixed at the subagent segment's earliest record timestamp.
	var mainClasses []orgClass
	for _, rs := range segs {
		if rs.ok && !rs.seg.Subagent {
			mainClasses = append(mainClasses, orgOf(rs.target.Location))
		}
	}
	for _, rs := range segs {
		if !rs.ok || !rs.seg.Subagent {
			continue
		}
		subClass := orgOf(rs.target.Location)
		for _, mc := range mainClasses {
			if crosses(mc, subClass) {
				mark(earliestSegmentTime(rs.seg))
				break
			}
		}
	}

	// Cache directory classification results (not "skip visited"), so an out-of-order
	// earlier subagent reach to the same org-B directory still moves earliest backward.
	cache := map[string]reachedClass{}
	classify := func(cand, baseRoot string) reachedClass {
		key := cand + "\x00" + baseRoot
		if hit, ok := cache[key]; ok {
			return hit
		}
		res := classifyReachedPath(cand, baseRoot)
		cache[key] = res
		return res
	}

	for _, rs := range segs {
		if !rs.ok || len(rs.seg.ToolReaches) == 0 {
			continue
		}
		base := orgOf(rs.target.Location)
		if !base.ok {
			continue
		}
		baseRoot := rs.target.Checkout.Root
		for _, tr := range rs.seg.ToolReaches {
			if found && !earliest.IsZero() && !tr.At.IsZero() && !tr.At.Before(earliest) {
				continue
			}
			expanded, unknown := hookrun.ExpandReach(tr.Paths, tr.Cwd)
			if unknown {
				mark(tr.At)
				continue
			}
			for _, p := range expanded {
				if !filepath.IsAbs(p) && tr.Cwd != "" {
					p = filepath.Join(tr.Cwd, p)
				}
				p = filepath.Clean(p)
				if !filepath.IsAbs(p) {
					continue
				}
				candidates := []string{p}
				if target, err := filepath.EvalSymlinks(p); err == nil && target != p {
					candidates = append(candidates, target)
				}
				crossed := false
				for _, cand := range candidates {
					cls := classify(cand, baseRoot)
					if cls.nobody {
						continue
					}
					if cls.unknown || crosses(base, orgOf(cls.loc)) {
						mark(tr.At)
						crossed = true
						break
					}
				}
				if crossed {
					break
				}
			}
		}
	}
	return earliest, found
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
	// Path no longer exists on disk: walk ancestors via gitx.LocateFS first so a deleted
	// subdirectory inside the segment's own repository (e.g. build/dist or removed pkg)
	// never falsely triggers Mixed, and a deleted path inside another existing repo still
	// resolves to that repo.
	if root, gitDir, ok := gitx.LocateFS(cand); ok && gitDir != "" {
		if baseRoot != "" && sameCleanPath(root, baseRoot) {
			return reachedClass{nobody: true}
		}
		if origin, _ := gitx.OriginFS(gitDir); routing.Owner(origin) != "" {
			return reachedClass{loc: routing.Location{Kind: routing.Repository, Repository: origin}}
		}
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

func earliestSegmentTime(seg agents.HistorySegment) time.Time {
	var best time.Time
	scan := func(msg *logspb.LogsData) {
		for _, rl := range msg.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					t := logRecordTime(lr)
					if !t.IsZero() && (best.IsZero() || t.Before(best)) {
						best = t
					}
				}
			}
		}
	}
	scan(seg.NativeLogs)
	scan(seg.SupplementalLogs)
	return best
}

func prepareSegmentFiltered(
	seg agents.HistorySegment,
	filter *relay.ContentFilter,
	pol config.Policy,
	stamp relay.StampOptions,
	snap runs.Snapshot,
	lookup func(string) runs.Snapshot,
	reachMixedAt time.Time,
	hasReachMixed bool,
	maxRecordTime time.Time,
) (*logspb.LogsData, int, int) {
	prepared, _ := PrepareSegment(seg, filter, pol, stamp)
	if prepared == nil {
		return nil, 0, 0
	}
	kept, dropped := FilterMixedLogs(prepared, snap, lookup, reachMixedAt, hasReachMixed, maxRecordTime)
	if kept == 0 {
		return nil, 0, dropped
	}
	return prepared, kept, dropped
}
