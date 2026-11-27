package history

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Options configures one Upload run.
type Options struct {
	StateDir         string
	Profile          string
	Account          string
	ServerKey        bool
	ServerKeyTeam    string
	ServerKeyAuthURL string
	AuthURL          string
	Version          string
	State            routing.State
	ReloadState      func() (routing.State, error)
	OnOrgAccessLost  func(orgID, authURL string, epochs map[string]int64, at time.Time)
	Sources          []agents.HistorySource
	Filter           *relay.ContentFilter
	Holdings         HoldingsChecker
	Transport        *Transport
	EndpointFor      func(teamID string) string
	ServerKeyFor     func(teamID string) (string, error)
	UserIDFor        func(team routing.Team, endpoint string) string
	TeamLabel        func(team routing.Team) string
	Since            time.Time
	// ActiveWindow is how recently a session's files may have changed for it to be left
	// out as still in progress; nil means DefaultActiveWindow, and zero leaves none out.
	ActiveWindow *time.Duration
	MaxReqBytes  int
	DryRun       bool
	RetryFailed  bool // re-send sessions a team rejected before, with the in-progress ones
	Now          func() time.Time
	Progress     func(ProgressEvent)
}

type routeSessionPlan struct {
	key            string
	file           agents.HistoryFile
	companions     []agents.HistoryFile
	missingParent  string
	source         agents.HistorySource
	records        int
	estimatedBytes int64
	sourceBytes    int64 // the session's files on disk, as the plan reports them
	overall        int   // 1-based position across every team's plan, 0 before the plan
}

type routeBucket struct {
	team     routing.Team
	label    string
	sessions map[string]*routeSessionPlan
	rejected int // sessions the server rejected permanently in a row
}

// Upload discovers past local sessions across all registered HistorySource agents, splits
// and routes every segment by its recorded working directory, checks server holdings per
// destination team, filters records through the relay content filter, and streams OTLP
// log chunks serially at 2 MiB/s per route while maintaining per-route resume state.
func Upload(ctx context.Context, opts Options) (Summary, error) {
	sum := newSummary(opts.DryRun)
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	activeWindow := DefaultActiveWindow
	if opts.ActiveWindow != nil {
		activeWindow = max(0, *opts.ActiveWindow)
	}
	maxReqBytes := opts.MaxReqBytes
	if maxReqBytes <= 0 {
		maxReqBytes = MaxRequestBytes
	}
	labelFor := func(t routing.Team) string {
		if opts.TeamLabel != nil {
			if l := opts.TeamLabel(t); l != "" {
				return l
			}
		}
		return t.TeamID
	}
	planTotal := 0
	report := func(ev ProgressEvent) {
		if opts.Progress == nil {
			return
		}
		if ev.Overall > 0 {
			ev.OverallTotal = planTotal
		}
		opts.Progress(ev)
	}

	if !opts.DryRun {
		unlock, err := LockUpload(opts.StateDir, opts.Account, opts.Profile)
		if err != nil {
			return sum, err
		}
		defer unlock()
	}

	userCache := map[string]string{}
	cachedUserID := func(team routing.Team, endpoint string) string {
		if opts.UserIDFor == nil {
			return ""
		}
		k := team.EnrollmentID + "\x00" + endpoint
		if v, ok := userCache[k]; ok {
			return v
		}
		v := opts.UserIDFor(team, endpoint)
		userCache[k] = v
		return v
	}

	existingStates, resumableByRoute, resumableKeys, err := preloadStates(opts)
	if err != nil {
		return sum, err
	}

	resolver := newCwdResolver(opts.State, opts.AuthURL, opts.ServerKey, opts.ServerKeyTeam, opts.ServerKeyAuthURL, now)
	reachCache := map[string]familyVerdict{}
	buckets, err := discoverAndPlan(ctx, opts, resolver, existingStates, resumableByRoute, resumableKeys, labelFor, cachedUserID, now, activeWindow, reachCache, &sum)
	if err != nil {
		return sum, err
	}
	report(ProgressEvent{Kind: ProgressDiscover, Total: sum.DiscoveredFiles, Skips: maps.Clone(sum.SkippedSessions)})

	enrollmentIDs := make([]string, 0, len(buckets))
	for id := range buckets {
		enrollmentIDs = append(enrollmentIDs, id)
	}
	slices.Sort(enrollmentIDs)

	// Every team's plan, and what its server already holds, is known before the first
	// session is sent, so the plan line can say how much is left in all.
	orgBlocked := map[string]string{}
	works := make([]*routeWork, 0, len(enrollmentIDs))
	for _, eid := range enrollmentIDs {
		if err := ctx.Err(); err != nil {
			sum.Interrupted = true
			return sum, err
		}
		w, err := planRoute(ctx, opts, existingStates, buckets[eid], nowFn, report, orgBlocked, &sum)
		if err != nil {
			return sum, err
		}
		works = append(works, w)
	}
	plan := buildUploadPlan(works, !opts.DryRun)
	planTotal = plan.Sessions
	report(ProgressEvent{Kind: ProgressPlan, Plan: &plan})
	if opts.DryRun {
		appendAgedOutFailures(opts.State.Teams, existingStates, labelFor, &sum)
		return sum, nil
	}

	for _, w := range works {
		if err := ctx.Err(); err != nil {
			sum.Interrupted = true
			return sum, err
		}
		if err := uploadRoute(ctx, opts, w, activeWindow, maxReqBytes, cachedUserID, nowFn, report, orgBlocked, reachCache, &sum); err != nil {
			return sum, err
		}
	}
	appendAgedOutFailures(opts.State.Teams, existingStates, labelFor, &sum)
	return sum, nil
}

func discoverAndPlan(
	ctx context.Context,
	opts Options,
	resolver *cwdResolver,
	existingStates map[string]RouteState,
	resumableByRoute map[string]map[string]bool,
	resumableKeys map[string]bool,
	labelFor func(routing.Team) string,
	userIDFor func(routing.Team, string) string,
	now time.Time,
	activeWindow time.Duration,
	reachCache map[string]familyVerdict,
	sum *Summary,
) (map[string]*routeBucket, error) {
	buckets := map[string]*routeBucket{}
	addBucketPlan := func(eid string, team routing.Team, key string, f agents.HistoryFile, companions []agents.HistoryFile, missingParent string, src agents.HistorySource, recs int, bytes int64) {
		b := buckets[eid]
		if b == nil {
			b = &routeBucket{team: team, label: labelFor(team), sessions: map[string]*routeSessionPlan{}}
			buckets[eid] = b
		}
		sp := b.sessions[key]
		if sp == nil {
			sp = &routeSessionPlan{key: key, file: f, companions: companions, missingParent: missingParent, source: src}
			b.sessions[key] = sp
		}
		sp.records += recs
		sp.estimatedBytes += bytes
	}
	for _, src := range opts.Sources {
		if err := ctx.Err(); err != nil {
			sum.Interrupted = true
			return nil, err
		}
		files, err := src.DiscoverHistory(ctx, agents.HistoryDiscoverOptions{
			Since:        opts.Since,
			Now:          now,
			ActiveWindow: activeWindow,
		})
		if err != nil {
			return nil, fmt.Errorf("discover %s history: %w", src.SourceSystem(), err)
		}
		bySession := indexBySession(files)
		childrenByParent := buildChildrenMap(files)
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				sum.Interrupted = true
				return nil, err
			}
			sum.DiscoveredFiles++
			key := SessionKey(f.SourceSystem, f.SessionID)
			companions, missingParent := collectFamily(f, bySession, childrenByParent)
			if f.Active || slices.ContainsFunc(companions, func(c agents.HistoryFile) bool { return c.Active }) {
				sum.addSkippedSession(SkipActive, 1)
				continue
			}
			if f.OutsideSince && !resumableKeys[key] {
				sum.addSkippedSession(SkipOutsideSince, 1)
				continue
			}
			if unreadable := familyReadError(f, companions); unreadable != "" {
				sum.addUnreadable(unreadable)
				sum.addSkippedSession(SkipUnreadable, 1)
				continue
			}
			type segSummary struct {
				target     RouteTarget
				ok         bool
				skipReason string
				records    int
			}
			var segSums []segSummary
			// Pass 1 never retains protobuf payloads in memory (even on --dry-run).
			err := src.ReadHistory(ctx, f, agents.HistoryReadOptions{IncludeSupplemental: false}, func(seg agents.HistorySegment) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				target, ok, skipReason := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
				if seg.RecordCount > 0 {
					segSums = append(segSums, segSummary{target: target, ok: ok, skipReason: skipReason, records: seg.RecordCount})
				}
				return nil
			})
			if err != nil {
				if errors.Is(err, context.Canceled) {
					sum.Interrupted = true
					return nil, err
				}
				if errors.Is(err, fs.ErrNotExist) {
					sum.addSkippedSession(SkipMissingFile, 1)
				} else {
					sum.addSkippedSession(SkipUnreadable, 1)
				}
				continue
			}
			anyRouted := false
			for _, rs := range segSums {
				if !rs.ok {
					if !f.OutsideSince {
						sum.addSkippedSegment(rs.skipReason, 1)
					}
					continue
				}
				eid := rs.target.Team.EnrollmentID
				if f.OutsideSince && !resumableByRoute[eid][key] {
					continue
				}
				anyRouted = true
				if !opts.DryRun {
					addBucketPlan(eid, rs.target.Team, key, f, companions, missingParent, src, rs.records, 0)
				}
			}
			if !opts.DryRun || !anyRouted {
				continue
			}
			snap, statSkip := statSessionFiles(f, companions)
			if statSkip != "" {
				sum.addSkippedSession(statSkip, 1)
				continue
			}
			reachMixedAt, hasReachMixed, cerr := familyCutoff(ctx, opts, src, resolver, f, companions, missingParent, snap, reachCache)
			if cerr != nil {
				if errors.Is(cerr, context.Canceled) {
					sum.Interrupted = true
					return nil, cerr
				}
				if errors.Is(cerr, fs.ErrNotExist) {
					sum.addSkippedSession(SkipMissingFile, 1)
				} else {
					sum.addSkippedSession(SkipUnreadable, 1)
				}
				continue
			}

			// Pass 2 (bounded memory) streams segments one at a time for dry-run estimates.
			byRoute, merr := measureDryRunSession(ctx, opts, src, f, key, resolver, existingStates, resumableByRoute, reachMixedAt, hasReachMixed, userIDFor, sum)
			if merr != nil {
				if errors.Is(merr, context.Canceled) {
					sum.Interrupted = true
					return nil, merr
				}
				if errors.Is(merr, fs.ErrNotExist) {
					sum.addSkippedSession(SkipMissingFile, 1)
				} else {
					sum.addSkippedSession(SkipUnreadable, 1)
				}
				continue
			}
			for eid, est := range byRoute {
				addBucketPlan(eid, est.team, key, f, companions, missingParent, src, est.records, est.bytes)
			}
		}
	}
	return buckets, nil
}
