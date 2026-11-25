package history

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
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

type cachedCompanionReach struct {
	segs         []segReachInfo
	ledgerCutoff time.Time
	hasCutoff    bool
}

func collectCompanionReach(
	ctx context.Context,
	stateDir string,
	src agents.HistorySource,
	resolver *cwdResolver,
	companions []agents.HistoryFile,
	missingParent string,
	cache map[string]cachedCompanionReach,
) ([]segReachInfo, time.Time, bool, error) {
	var out []segReachInfo
	var ledgerCutoff time.Time
	hasLedgerCutoff := false
	farPast := time.Unix(1, 0).UTC()
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if missingParent != "" {
		// Parent transcript is missing from disk: fail closed rather than uploading a
		// spawned child whose parent may have crossed organizations before spawning it.
		ledgerCutoff, hasLedgerCutoff = time.Time{}, true
	}
	stateKey := ""
	if resolver != nil {
		stateKey = fmt.Sprintf("%s:%d:%d", resolver.state.StateID, resolver.state.Generation, resolver.state.ConsentGeneration)
	}
	for _, comp := range companions {
		if err := ctx.Err(); err != nil {
			return nil, time.Time{}, false, err
		}
		cacheKey := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", stateKey, comp.SourceSystem, comp.SessionID, comp.Path, comp.ModTime.UnixNano())
		if cache != nil {
			if hit, ok := cache[cacheKey]; ok {
				out = append(out, hit.segs...)
				ledgerCutoff, hasLedgerCutoff = mergeMixedCutoff(ledgerCutoff, hasLedgerCutoff, hit.ledgerCutoff, hit.hasCutoff)
				continue
			}
		}
		var compSegs []segReachInfo
		var compCutoff time.Time
		hasCompCutoff := false
		if mAt, mixed := runs.Read(stateDir, comp.SessionID).MixedInterval(farPast, farFuture); mixed {
			compCutoff, hasCompCutoff = mergeMixedCutoff(compCutoff, hasCompCutoff, mAt, true)
		}
		isChild := comp.ParentSessionID != ""
		err := src.ReadHistory(ctx, comp, agents.HistoryReadOptions{IncludeSupplemental: false}, func(seg agents.HistorySegment) error {
			target, ok, _ := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
			compSegs = append(compSegs, segReachInfo{
				cwd:      seg.Cwd,
				subagent: isChild || seg.Subagent,
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
			// Fail closed when any transcript in the spawn family cannot be read.
			compCutoff, hasCompCutoff = time.Time{}, true
		}
		if cache != nil {
			cache[cacheKey] = cachedCompanionReach{segs: compSegs, ledgerCutoff: compCutoff, hasCutoff: hasCompCutoff}
		}
		out = append(out, compSegs...)
		ledgerCutoff, hasLedgerCutoff = mergeMixedCutoff(ledgerCutoff, hasLedgerCutoff, compCutoff, hasCompCutoff)
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

	type timedOrg struct {
		at  time.Time
		org orgClass
	}
	var mainTimed, subTimed []timedOrg
	var unknownMainAt time.Time
	hasUnknownMain, hasSubagent := false, false
	for _, rs := range segs {
		if rs.subagent {
			hasSubagent = true
			if oc := orgOf(rs.target.Location); oc.ok {
				subTimed = append(subTimed, timedOrg{at: rs.earliest, org: oc})
			}
			continue
		}
		if rs.target.Location.Kind == "" {
			if rs.cwd != "" {
				unknownMainAt, hasUnknownMain = mergeMixedCutoff(unknownMainAt, hasUnknownMain, rs.earliest, true)
			}
			continue
		}
		if oc := orgOf(rs.target.Location); oc.ok {
			mainTimed = append(mainTimed, timedOrg{at: rs.earliest, org: oc})
		}
	}
	slices.SortStableFunc(mainTimed, func(a, b timedOrg) int { return a.at.Compare(b.at) })

	// mainInForceAt returns the main-segment organization(s) active at timestamp at (the
	// latest main segment with m.at <= at, falling back to the earliest main segment).
	// Comparing subagents only against the main placement in force when they ran avoids
	// false Mixed marks when a parent finishes in org A and later resumes in org B.
	mainInForceAt := func(at time.Time) []orgClass {
		if len(mainTimed) == 0 {
			return nil
		}
		if at.IsZero() {
			out := make([]orgClass, len(mainTimed))
			for i, m := range mainTimed {
				out[i] = m.org
			}
			return out
		}
		chosen := mainTimed[0].at
		for _, m := range mainTimed {
			if !m.at.After(at) {
				chosen = m.at
			} else {
				break
			}
		}
		var out []orgClass
		for _, m := range mainTimed {
			if m.at.Equal(chosen) {
				out = append(out, m.org)
			}
		}
		return out
	}

	// If a main segment had an unresolvable cwd (deleted repo or origin mismatch) while
	// subagents exist, fail closed because the parent's organization is unknown.
	if hasUnknownMain && hasSubagent {
		mark(unknownMainAt)
	}
	for _, rs := range segs {
		if !rs.subagent {
			continue
		}
		if rs.target.Location.Kind == "" {
			if rs.cwd != "" && (len(mainTimed) > 0 || len(subTimed) > 0) {
				mark(rs.earliest)
			}
			continue
		}
		subClass := orgOf(rs.target.Location)
		for _, mc := range mainInForceAt(rs.earliest) {
			if crosses(mc, subClass) {
				mark(rs.earliest)
				break
			}
		}
	}
	// Sibling subagents running under the same main placement (including when the main
	// session has no resolved org, e.g. OutsideGit with no Personal team) must not span
	// different organizations.
	for i := range subTimed {
		mi := mainInForceAt(subTimed[i].at)
		for j := i + 1; j < len(subTimed); j++ {
			if !crosses(subTimed[i].org, subTimed[j].org) {
				continue
			}
			mj := mainInForceAt(subTimed[j].at)
			if len(mi) > 0 && len(mj) > 0 && !slices.Equal(mi, mj) {
				continue
			}
			at := subTimed[j].at
			if subTimed[i].at.IsZero() || subTimed[j].at.IsZero() {
				at = time.Time{}
			} else if subTimed[i].at.After(at) {
				at = subTimed[i].at
			}
			mark(at)
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

	allSubOrgs := make([]orgClass, len(subTimed))
	for i, s := range subTimed {
		allSubOrgs[i] = s.org
	}
	for _, rs := range segs {
		if len(rs.reaches) == 0 {
			continue
		}
		segOrg := orgOf(rs.target.Location)
		baseRoot := cmp.Or(rs.target.Checkout.Root, rs.target.Checkout.Cwd)
		for _, tr := range rs.reaches {
			if found && !earliest.IsZero() && !tr.At.IsZero() && !tr.At.Before(earliest) {
				continue
			}
			var bases []orgClass
			switch {
			case rs.subagent:
				inForce := mainInForceAt(cmp.Or(tr.At, rs.earliest))
				bases = append(bases, inForce...)
				if segOrg.ok {
					bases = append(bases, segOrg)
				}
				if len(inForce) == 0 {
					bases = append(bases, allSubOrgs...)
				}
			case segOrg.ok:
				bases = []orgClass{segOrg}
			default:
				// Unrouted main segment (e.g. OutsideGit without Personal, or missing_cwd):
				// compare its tool reaches against the active main org and all subagent orgs.
				bases = append(bases, mainInForceAt(cmp.Or(tr.At, rs.earliest))...)
				bases = append(bases, allSubOrgs...)
			}
			if len(bases) == 0 {
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
