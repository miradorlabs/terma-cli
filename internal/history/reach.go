package history

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// segReachInfo is what the Mixed check needs from one transcript segment: where it ran,
// what its tool calls reached, and when it started.
type segReachInfo struct {
	cwd      string
	reaches  []agents.HistoryToolReach
	earliest time.Time
	target   RouteTarget
}

type reachedClass struct {
	loc     routing.Location
	nobody  bool
	unknown bool
}

// orgClass is the organization a location belongs to, as routing.Router.OrganizationOf
// reports it, plus the repository so a pending binding compares equal to itself.
type orgClass struct {
	org, repo   string
	pending, ok bool
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

func (r *cwdResolver) orgOf(loc routing.Location) orgClass {
	if loc.Kind == "" {
		return orgClass{}
	}
	org, pending, ok := r.router.OrganizationOf(loc, r.now)
	repo := ""
	if loc.Kind == routing.Repository {
		repo = routing.Normalize(loc.Repository)
	}
	return orgClass{org: org, repo: repo, pending: pending, ok: ok}
}

// crosses mirrors Routing.crossing in internal/hooks/hookrun/placement.go.
func crosses(a, b orgClass) bool {
	if !a.ok || !b.ok {
		return false
	}
	if (a.pending || b.pending) && a.repo != "" && a.repo == b.repo {
		return false
	}
	return a.pending || b.pending || a.org != b.org
}

// familyVerdict is what a spawn family's transcripts say about its Mixed cutoff, for the
// file snapshot and routing state it was read under.
type familyVerdict struct {
	snap  fileSnapshot
	state string
	at    time.Time
	mixed bool
}

// familyCutoff returns the Mixed cutoff for the spawn family of f (f plus companions):
// the transcripts' verdict from familyMixedAt, merged with every companion's live run
// ledger. The transcript verdict is cached per family, file snapshot and routing state, so
// each member of a family reads the family once; the ledgers are read on every call, since
// a live hook can mark a run Mixed between two members' uploads. An error reading f itself
// is returned; a companion that cannot be read withholds everything.
func familyCutoff(
	ctx context.Context,
	opts Options,
	src agents.HistorySource,
	resolver *cwdResolver,
	f agents.HistoryFile,
	companions []agents.HistoryFile,
	missingParent string,
	snap fileSnapshot,
	cache map[string]familyVerdict,
) (time.Time, bool, error) {
	if missingParent != "" {
		// A child whose parent transcript is gone: the parent may have crossed
		// organizations before spawning it.
		return time.Time{}, true, nil
	}
	ids := []string{f.SessionID}
	for _, c := range companions {
		ids = append(ids, c.SessionID)
	}
	slices.Sort(ids)
	key := f.SourceSystem + "\x00" + strings.Join(ids, "\x00")
	state := fmt.Sprintf("%s:%d:%d", resolver.state.StateID, resolver.state.Generation, resolver.state.ConsentGeneration)

	var at time.Time
	var mixed bool
	if hit, ok := cache[key]; ok && hit.snap == snap && hit.state == state {
		at, mixed = hit.at, hit.mixed
	} else {
		subagents := len(companions) > 0 || f.ParentSessionID != ""
		var segs []segReachInfo
		collect := func(seg agents.HistorySegment) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			subagents = subagents || seg.Subagent
			target, _, _ := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
			segs = append(segs, segReachInfo{cwd: seg.Cwd, reaches: seg.ToolReaches, earliest: earliestSegmentTime(seg), target: target})
			return nil
		}
		if err := src.ReadHistory(ctx, f, agents.HistoryReadOptions{IncludeSupplemental: false}, collect); err != nil {
			return time.Time{}, false, err
		}
		readAll := true
		for _, comp := range companions {
			if err := src.ReadHistory(ctx, comp, agents.HistoryReadOptions{IncludeSupplemental: false}, collect); err != nil {
				if errors.Is(err, context.Canceled) {
					return time.Time{}, false, err
				}
				readAll = false
				break
			}
		}
		if readAll {
			at, mixed = familyMixedAt(resolver, segs, subagents)
			if cache != nil {
				cache[key] = familyVerdict{snap: snap, state: state, at: at, mixed: mixed}
			}
		} else {
			at, mixed = time.Time{}, true
		}
	}

	farPast := time.Unix(1, 0).UTC()
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, comp := range companions {
		if mAt, m := runs.Read(opts.StateDir, comp.SessionID).MixedInterval(farPast, farFuture); m {
			at, mixed = mergeMixedCutoff(at, mixed, mAt, true)
		}
	}
	return at, mixed, nil
}

// familyMixedAt returns the earliest moment from which a spawn family's content must be
// withheld; a zero time with true withholds everything. Two rules apply.
//
// A family with subagents must sit in one organization, because a subagent's output flows
// into its parent at times the transcripts do not pin down. So when the segments' working
// directories span two organizations, the family is cut from when the second one appears,
// and a working directory that can no longer be placed cuts it from that segment's start.
// The cost is deliberate: a parent that later resumes in another organization is withheld
// from then on even if its subagents had finished. That needs two organizations,
// subagents, and a session resumed across them, so it is rare, and it fails closed.
//
// Every tool call's reach is compared with its own segment's organization, or with every
// organization in the family when the segment's own cannot be placed. A transcript without
// subagents that resumes in a second repository therefore still splits cleanly by working
// directory.
func familyMixedAt(resolver *cwdResolver, segs []segReachInfo, subagents bool) (time.Time, bool) {
	if resolver == nil || len(segs) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	found := false
	mark := func(at time.Time) {
		earliest, found = mergeMixedCutoff(earliest, found, at, true)
	}

	// Segments are flushed in fixed-size chunks, so compare distinct organizations rather
	// than segment pairs. A class whose start is unknown (zero) in any segment is unknown.
	var classes []orgClass
	starts := map[orgClass]time.Time{}
	for _, rs := range segs {
		oc := resolver.orgOf(rs.target.Location)
		if !oc.ok {
			if subagents && rs.target.Location.Kind == "" && rs.cwd != "" {
				mark(rs.earliest)
			}
			continue
		}
		start, seen := starts[oc]
		switch {
		case !seen:
			classes = append(classes, oc)
			start = rs.earliest
		case start.IsZero() || rs.earliest.IsZero():
			start = time.Time{}
		case rs.earliest.Before(start):
			start = rs.earliest
		}
		starts[oc] = start
	}
	if subagents {
		for i, a := range classes {
			for _, b := range classes[i+1:] {
				if !crosses(a, b) {
					continue
				}
				sa, sb := starts[a], starts[b]
				switch {
				case sa.IsZero() || sb.IsZero():
					mark(time.Time{})
				case sa.After(sb):
					mark(sa)
				default:
					mark(sb)
				}
			}
		}
	}

	// Cache classification results per call (not "skip visited"), so an earlier reach to
	// the same directory still moves the cutoff backward.
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
		bases := classes
		if own := resolver.orgOf(rs.target.Location); own.ok {
			bases = []orgClass{own}
		}
		if len(bases) == 0 {
			continue
		}
		baseRoot := rs.target.Checkout.Root
		// A segment placed by the origin its transcript recorded, because its checkout is
		// since deleted, has no root on disk: what it reached under its own working
		// directory was that checkout. A working directory outside git gets no such pass.
		goneRoot := ""
		if baseRoot == "" && rs.target.Location.Kind == routing.Repository {
			goneRoot = rs.target.Checkout.Cwd
		}
		for _, tr := range rs.reaches {
			if found && !earliest.IsZero() && !tr.At.IsZero() && !tr.At.Before(earliest) {
				continue
			}
			if reachCrosses(tr, baseRoot, goneRoot, bases, resolver, classify) {
				mark(tr.At)
			}
		}
	}
	return earliest, found
}

// reachCrosses reports whether one tool call reached somewhere whose organization is
// unknown or crosses one of bases. Paths under goneRoot, the segment's own deleted
// checkout, are its own.
func reachCrosses(tr agents.HistoryToolReach, baseRoot, goneRoot string, bases []orgClass, resolver *cwdResolver, classify func(cand, baseRoot string) reachedClass) bool {
	expanded, unknown := hookrun.ExpandReachHistorical(tr.Paths, tr.Cwd)
	if unknown {
		return true
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
		for _, cand := range candidates {
			if goneRoot != "" && pathUnder(cand, goneRoot) {
				continue
			}
			cls := classify(cand, baseRoot)
			if cls.nobody {
				continue
			}
			if cls.unknown {
				return true
			}
			reached := resolver.orgOf(cls.loc)
			if slices.ContainsFunc(bases, func(b orgClass) bool { return crosses(b, reached) }) {
				return true
			}
		}
	}
	return false
}

// pathUnder reports whether path is root or lies below it, by name alone.
func pathUnder(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// earliestSegmentTime returns when a segment started: the earliest timestamp among its log
// records and tool reaches, or zero when any of them is untimed (its start is then unknown).
func earliestSegmentTime(seg agents.HistorySegment) time.Time {
	var best time.Time
	untimed := false
	consider := func(t time.Time) {
		if t.IsZero() {
			untimed = true
		} else if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	scan := func(msg *logspb.LogsData) {
		for _, rl := range msg.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					consider(logRecordTime(lr))
				}
			}
		}
	}
	scan(seg.NativeLogs)
	scan(seg.SupplementalLogs)
	for _, tr := range seg.ToolReaches {
		consider(tr.At)
	}
	if untimed {
		return time.Time{}
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
