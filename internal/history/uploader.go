package history

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
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
	ActiveWindow     time.Duration
	MaxReqBytes      int
	DryRun           bool
	Now              func() time.Time
	Progress         func(ProgressEvent)
}

type routeSessionPlan struct {
	key            string
	file           agents.HistoryFile
	source         agents.HistorySource
	records        int
	estimatedBytes int64
}

type routeBucket struct {
	team     routing.Team
	label    string
	sessions map[string]*routeSessionPlan
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
	activeWindow := opts.ActiveWindow
	if activeWindow <= 0 {
		activeWindow = time.Hour
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
	report := func(ev ProgressEvent) {
		if opts.Progress != nil {
			opts.Progress(ev)
		}
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
	buckets, err := discoverAndPlan(ctx, opts, resolver, existingStates, resumableByRoute, resumableKeys, labelFor, cachedUserID, now, activeWindow, &sum)
	if err != nil {
		return sum, err
	}
	report(ProgressEvent{Kind: ProgressDiscover, Total: sum.DiscoveredFiles})

	enrollmentIDs := make([]string, 0, len(buckets))
	for id := range buckets {
		enrollmentIDs = append(enrollmentIDs, id)
	}
	slices.Sort(enrollmentIDs)

	orgBlocked := map[string]string{}
	for _, eid := range enrollmentIDs {
		if err := ctx.Err(); err != nil {
			sum.Interrupted = true
			return sum, err
		}
		if err := processRoute(ctx, opts, existingStates, buckets[eid], activeWindow, maxReqBytes, cachedUserID, nowFn, report, orgBlocked, &sum); err != nil {
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
	sum *Summary,
) (map[string]*routeBucket, error) {
	buckets := map[string]*routeBucket{}
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
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				sum.Interrupted = true
				return nil, err
			}
			sum.DiscoveredFiles++
			key := SessionKey(f.SourceSystem, f.SessionID)
			if f.Active {
				sum.addSkippedSession(SkipActive, 1)
				continue
			}
			if f.OutsideSince && !resumableKeys[key] {
				sum.addSkippedSession(SkipOutsideSince, 1)
				continue
			}
			var runSnap runs.Snapshot
			var subSnaps map[string]runs.Snapshot
			runSnapLoaded := false
			getRunSnap := func() (runs.Snapshot, func(string) runs.Snapshot) {
				if !runSnapLoaded {
					runSnapLoaded = true
					runSnap = runs.Read(opts.StateDir, f.SessionID)
					subSnaps = map[string]runs.Snapshot{}
				}
				return runSnap, func(id string) runs.Snapshot {
					if id == f.SessionID {
						return runSnap
					}
					if s, ok := subSnaps[id]; ok {
						return s
					}
					s := runs.Read(opts.StateDir, id)
					subSnaps[id] = s
					return s
				}
			}
			var resolvedSegs []resolvedSegment
			err := src.ReadHistory(ctx, f, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				target, ok, skipReason := resolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
				resolvedSegs = append(resolvedSegs, resolvedSegment{seg: seg, target: target, ok: ok, skipReason: skipReason})
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
			var reachMixedAt time.Time
			var hasReachMixed bool
			if opts.DryRun {
				reachMixedAt, hasReachMixed = sessionReachMixedAt(resolver, resolvedSegs)
			}
			for _, rs := range resolvedSegs {
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
				recCount := rs.seg.RecordCount
				var estBytes int64
				st, hasState := existingStates[eid]
				_, prevFailed := st.IsFailed(key)
				alreadySettled := hasState && (st.IsDone(key) || prevFailed)
				if !alreadySettled && opts.DryRun {
					snap, lookup := getRunSnap()
					var resumeMax time.Time
					if prev, ok := st.InProgress[key]; ok && !prev.SourceModTime.IsZero() && f.ModTime.After(prev.SourceModTime) {
						resumeMax = prev.SourceModTime
					}
					prepared, n, mixedDropped := prepareSegmentFiltered(rs.seg, opts.Filter, rs.target.Team.Policy, buildStampOptions(opts, rs.target, userIDFor), snap, lookup, reachMixedAt, hasReachMixed, resumeMax)
					if n == 0 || prepared == nil {
						if mixedDropped > 0 {
							sum.addSkippedSegment(SkipMixed, 1)
						} else {
							sum.addSkippedSegment(SkipEmptyAfterFilter, 1)
						}
						continue
					}
					recCount = n
					estBytes = int64(proto.Size(prepared))
				}
				b := buckets[eid]
				if b == nil {
					b = &routeBucket{
						team:     rs.target.Team,
						label:    labelFor(rs.target.Team),
						sessions: map[string]*routeSessionPlan{},
					}
					buckets[eid] = b
				}
				sp := b.sessions[key]
				if sp == nil {
					sp = &routeSessionPlan{key: key, file: f, source: src}
					b.sessions[key] = sp
				}
				sp.records += recCount
				sp.estimatedBytes += estBytes
			}
		}
	}
	return buckets, nil
}

func processRoute(
	ctx context.Context,
	opts Options,
	existingStates map[string]RouteState,
	b *routeBucket,
	activeWindow time.Duration,
	maxReqBytes int,
	userIDFor func(routing.Team, string) string,
	nowFn func() time.Time,
	report func(ProgressEvent),
	orgBlocked map[string]string,
	sum *Summary,
) error {
	eid := b.team.EnrollmentID
	st, ok := existingStates[eid]
	if !ok {
		var err error
		st, _, err = LoadState(opts.StateDir, opts.Account, opts.Profile, eid, b.team.TeamID)
		if err != nil {
			return err
		}
	}

	var resumeQueue, candidateQueue []*routeSessionPlan
	for _, sp := range sortedPlans(b.sessions) {
		if st.IsDone(sp.key) {
			sum.addSkippedSession(SkipAlreadyDone, 1)
			continue
		}
		if frec, failed := st.IsFailed(sp.key); failed {
			sum.addSkippedSession(SkipFailedPreviously, 1)
			sum.Failed = append(sum.Failed, FailedSession{
				EnrollmentID:    eid,
				TeamID:          b.team.TeamID,
				TeamLabel:       b.label,
				SourceSystem:    frec.SourceSystem,
				SessionID:       frec.SessionID,
				Reason:          frec.Reason,
				RejectedRecords: frec.RejectedRecords,
				Previously:      true,
				FailedAt:        frec.FailedAt,
			})
			continue
		}
		if st.IsInProgress(sp.key) {
			resumeQueue = append(resumeQueue, sp)
		} else {
			candidateQueue = append(candidateQueue, sp)
		}
	}

	rp := RoutePlan{
		EnrollmentID:   eid,
		TeamID:         b.team.TeamID,
		TeamLabel:      b.label,
		Sessions:       len(resumeQueue) + len(candidateQueue),
		ResumeSessions: len(resumeQueue),
	}
	for _, sp := range resumeQueue {
		rp.Records += sp.records
		rp.EstimatedBytes += sp.estimatedBytes
	}
	for _, sp := range candidateQueue {
		rp.Records += sp.records
		rp.EstimatedBytes += sp.estimatedBytes
	}
	if rp.Sessions > 0 {
		sum.Planned = append(sum.Planned, rp)
	}
	if opts.DryRun || rp.Sessions == 0 {
		return nil
	}

	if reason, blocked := orgBlocked[b.team.OrganizationID]; blocked {
		totalSkip := len(resumeQueue) + len(candidateQueue)
		sum.addSkippedSession(SkipOrgAccessLost, totalSkip)
		sum.TeamIssues = append(sum.TeamIssues, TeamIssue{
			EnrollmentID: eid, TeamID: b.team.TeamID, TeamLabel: b.label,
			Skipped: totalSkip, Reason: reason,
		})
		return nil
	}

	pacer := opts.Transport.NewRoutePacer()
	endpoint := ""
	if opts.EndpointFor != nil {
		endpoint = opts.EndpointFor(b.team.TeamID)
	}
	serverKey, keyOK := resolveRouteServerKey(opts, b, len(resumeQueue)+len(candidateQueue), report, sum)
	if !keyOK {
		return nil
	}

	// Re-send in_progress sessions FIRST and skip holdings check for them.
	for i, sp := range resumeQueue {
		stopRoute, retErr := uploadOneSession(ctx, opts, &st, b, sp, pacer, endpoint, serverKey, activeWindow, maxReqBytes, i+1, len(resumeQueue), len(candidateQueue), userIDFor, nowFn, report, sum, orgBlocked)
		if retErr != nil {
			return retErr
		}
		if stopRoute {
			return nil
		}
	}

	if len(candidateQueue) == 0 {
		return nil
	}
	freshState, _, checkSkip, checkReason, routeOK := recheckRouteTeam(opts, eid, nowFn())
	if !routeOK {
		stopRouteWithIssue(b, len(candidateQueue), checkSkip, checkReason, report, sum)
		return nil
	}
	uploadQueue, err := filterHeldCandidates(ctx, opts, freshState, b, candidateQueue, nowFn, report, orgBlocked, sum)
	if err != nil {
		return err
	}
	for i, sp := range uploadQueue {
		stopRoute, retErr := uploadOneSession(ctx, opts, &st, b, sp, pacer, endpoint, serverKey, activeWindow, maxReqBytes, i+1, len(uploadQueue), 0, userIDFor, nowFn, report, sum, orgBlocked)
		if retErr != nil {
			return retErr
		}
		if stopRoute {
			return nil
		}
	}
	return nil
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
