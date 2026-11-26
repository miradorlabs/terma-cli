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

// statHistoryFile snapshots a transcript and its companion files, returning a skip reason
// when the session cannot be uploaded as discovered: SkipMissingFile when a known file is
// gone or not a regular file, SkipUnreadable when a file or watch directory cannot be
// inspected, and SkipActive when a watch directory holds a transcript discovery did not
// list (a companion that appeared since, which no pass would read).
func statHistoryFile(f agents.HistoryFile) (fileSnapshot, string) {
	if f.Path == "" {
		return fileSnapshot{modTime: f.ModTime}, ""
	}
	var snap fileSnapshot
	known := map[string]bool{}
	add := func(path string) string {
		clean := filepath.Clean(path)
		if known[clean] {
			return ""
		}
		known[clean] = true
		fi, err := os.Lstat(clean)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return SkipUnreadable
		}
		if err != nil || !fi.Mode().IsRegular() {
			return SkipMissingFile
		}
		if fi.ModTime().After(snap.modTime) {
			snap.modTime = fi.ModTime()
		}
		snap.size += fi.Size()
		snap.count++
		return ""
	}
	for _, p := range append([]string{f.Path}, f.ExtraPaths...) {
		if reason := add(p); reason != "" {
			return fileSnapshot{}, reason
		}
	}
	for _, dir := range f.WatchDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fileSnapshot{}, SkipUnreadable
		}
		for _, ent := range entries {
			if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, ent.Name())
			if known[path] {
				continue
			}
			fi, err := os.Lstat(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return fileSnapshot{}, SkipUnreadable
			case fi.Mode().IsRegular():
				return fileSnapshot{}, SkipActive
			}
		}
	}
	return snap, ""
}

// statSessionFiles snapshots f and its spawn family, returning the first skip reason any
// of them reports (see statHistoryFile).
func statSessionFiles(f agents.HistoryFile, descendants []agents.HistoryFile) (fileSnapshot, string) {
	snap, reason := statHistoryFile(f)
	if reason != "" {
		return fileSnapshot{}, reason
	}
	for _, d := range descendants {
		ds, reason := statHistoryFile(d)
		if reason != "" {
			return fileSnapshot{}, reason
		}
		if ds.modTime.After(snap.modTime) {
			snap.modTime = ds.modTime
		}
		snap.size += ds.size
		snap.count += ds.count
	}
	return snap, ""
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
	// Chunk each route's batches as the upload would, so the estimate counts request
	// bodies and a record over the request limit leaves the session out of that route.
	writers := map[string]*ChunkWriter{}
	tooLarge := map[string]bool{}
	writerFor := func(eid string) *ChunkWriter {
		if w := writers[eid]; w != nil {
			return w
		}
		w := NewChunkWriter(opts.MaxReqBytes, func(ch Chunk) error {
			cur := byRoute[eid]
			cur.bytes += int64(len(ch.Body))
			byRoute[eid] = cur
			return nil
		})
		writers[eid] = w
		return w
	}
	noteChunkErr := func(eid string, err error) error {
		if _, ok := errors.AsType[*PermanentError](err); ok {
			tooLarge[eid] = true
			return nil
		}
		return err
	}
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
		if hasState && (st.IsDone(key) || prevFailed && !opts.RetryFailed) {
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
		sum.WithheldRecords += mixedDropped
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
		byRoute[eid] = cur
		if tooLarge[eid] {
			return nil
		}
		return noteChunkErr(eid, writerFor(eid).Add(prepared))
	})
	if err != nil {
		return byRoute, err
	}
	for eid, w := range writers {
		if tooLarge[eid] {
			continue
		}
		if err := noteChunkErr(eid, w.Flush()); err != nil {
			return byRoute, err
		}
	}
	for eid := range tooLarge {
		delete(byRoute, eid)
		sum.addSkippedSession(SkipRecordTooLarge, 1)
	}
	return byRoute, nil
}

// classifyReachedPath classifies a reached path without spawning git subprocesses.
// Uses gitx.LocateFS so deleted subdirectories inside an existing checkout still resolve
// to their enclosing repository (baseRoot, the segment's own checkout root, counts as
// nobody's), while any other missing path outside nobody's directories fails closed
// (unknown=true), even under a working directory outside git: a deleted organization
// repository there must not read as the developer's own folder.
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
	// IsNobodyDir goes by name, so it covers a missing path under /tmp or /etc. The parent
	// is not asked: the parent of a deleted top-level directory is "/", which is nobody's.
	if hookrun.IsNobodyDir(cand) {
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
