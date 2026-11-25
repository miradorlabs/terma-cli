package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

type segReachInfo struct {
	cwd      string
	subagent bool
	reaches  []agents.HistoryToolReach
	earliest time.Time
	target   RouteTarget
	ok       bool
}

type reachedClass struct {
	loc     routing.Location
	nobody  bool
	unknown bool
}

func mergeMixedCutoff(cur time.Time, hasCur bool, at time.Time, hasAt bool) (time.Time, bool) {
	if !hasAt {
		return cur, hasCur
	}
	if !hasCur || at.IsZero() || (!cur.IsZero() && at.Before(cur)) {
		return at, true
	}
	return cur, true
}

func collectDescendantReach(
	ctx context.Context,
	stateDir string,
	src agents.HistorySource,
	resolver *cwdResolver,
	descendants []agents.HistoryFile,
) ([]segReachInfo, time.Time, bool, error) {
	var out []segReachInfo
	var ledgerCutoff time.Time
	hasLedgerCutoff := false
	farPast := time.Unix(1, 0).UTC()
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, desc := range descendants {
		if err := ctx.Err(); err != nil {
			return nil, time.Time{}, false, err
		}
		if mAt, mixed := runs.Read(stateDir, desc.SessionID).MixedInterval(farPast, farFuture); mixed {
			ledgerCutoff, hasLedgerCutoff = mergeMixedCutoff(ledgerCutoff, hasLedgerCutoff, mAt, true)
		}
		err := src.ReadHistory(ctx, desc, agents.HistoryReadOptions{IncludeSupplemental: false}, func(seg agents.HistorySegment) error {
			target, ok, _ := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
			out = append(out, segReachInfo{
				cwd:      seg.Cwd,
				subagent: true,
				reaches:  seg.ToolReaches,
				earliest: earliestSegmentTime(seg),
				target:   target,
				ok:       ok,
			})
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, time.Time{}, false, err
			}
			// Fail closed when a spawned child transcript cannot be read.
			ledgerCutoff, hasLedgerCutoff = time.Time{}, true
		}
	}
	return out, ledgerCutoff, hasLedgerCutoff, nil
}

// sessionReachMixedAt inspects every segment of a session (including unrouted/blocked
// segments and companion/spawned subagents) using historical static expansion, the nobodys
// filter, and routing.Router.OrganizationOf. Returning one earliest cutoff ensures cross-org
// reach is never reset by a later cwd change or hidden by held/uncollected subagents.
func sessionReachMixedAt(resolver *cwdResolver, segs []segReachInfo) (time.Time, bool) {
	if resolver == nil || len(segs) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	found := false
	mark := func(at time.Time) {
		earliest, found = mergeMixedCutoff(earliest, found, at, true)
	}

	type orgClass struct {
		org, repo   string
		pending, ok bool
	}
	orgOf := func(loc routing.Location) orgClass {
		if loc.Kind == "" {
			return orgClass{}
		}
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

	// Collect known organizations across main segments (including blocked/bound-not-collected
	// checkouts) so subagents and unrouted segments compare against them.
	var mainClasses []orgClass
	for _, rs := range segs {
		if !rs.subagent {
			if oc := orgOf(rs.target.Location); oc.ok {
				mainClasses = append(mainClasses, oc)
			}
		}
	}
	for _, rs := range segs {
		if !rs.subagent {
			continue
		}
		if rs.target.Location.Kind == "" {
			if rs.cwd != "" && len(mainClasses) > 0 {
				mark(rs.earliest)
			}
			continue
		}
		subClass := orgOf(rs.target.Location)
		for _, mc := range mainClasses {
			if crosses(mc, subClass) {
				mark(rs.earliest)
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
		if len(rs.reaches) == 0 {
			continue
		}
		segOrg := orgOf(rs.target.Location)
		var bases []orgClass
		if rs.subagent {
			bases = mainClasses
			if segOrg.ok {
				bases = append(slices.Clone(mainClasses), segOrg)
			}
		} else if segOrg.ok {
			bases = []orgClass{segOrg}
		} else {
			// Unrouted main segment (e.g. OutsideGit without Personal, or missing_cwd):
			// compare its tool reaches against every known main org in the session.
			bases = mainClasses
		}
		if len(bases) == 0 {
			continue
		}
		baseRoot := rs.target.Checkout.Root
		for _, tr := range rs.reaches {
			if found && !earliest.IsZero() && !tr.At.IsZero() && !tr.At.Before(earliest) {
				continue
			}
			expanded, unknown := hookrun.ExpandReachHistorical(tr.Paths, tr.Cwd)
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
					if cls.unknown {
						mark(tr.At)
						crossed = true
						break
					}
					reachedOrg := orgOf(cls.loc)
					for _, b := range bases {
						if crosses(b, reachedOrg) {
							mark(tr.At)
							crossed = true
							break
						}
					}
					if crossed {
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
	for _, tr := range seg.ToolReaches {
		if !tr.At.IsZero() && (best.IsZero() || tr.At.Before(best)) {
			best = tr.At
		}
	}
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
