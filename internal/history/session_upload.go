package history

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

type preAttemptStopError struct {
	skipReason string
	reason     string
	active     bool
}

func (e *preAttemptStopError) Error() string { return e.reason }

func uploadOneSession(
	ctx context.Context,
	opts Options,
	st *RouteState,
	b *routeBucket,
	sp *routeSessionPlan,
	pacer *RoutePacer,
	endpoint, serverKey string,
	activeWindow time.Duration,
	maxReqBytes, idx, total, extraRemaining int,
	userIDFor func(routing.Team, string) string,
	nowFn func() time.Time,
	report func(ProgressEvent),
	sum *Summary,
	orgBlocked map[string]string,
) (stopRoute bool, fatalErr error) {
	if err := ctx.Err(); err != nil {
		sum.Interrupted = true
		return true, err
	}
	remaining := max(1, total-idx+1) + max(0, extraRemaining)

	// Re-stat transcript files right before reading so a session resumed while earlier
	// routes were draining is never uploaded mid-write or marked done prematurely.
	preSnap, exists := statHistoryFile(sp.file)
	if !exists {
		sum.addSkippedSession(SkipMissingFile, 1)
		return false, nil
	}
	now := nowFn()
	if now.Sub(preSnap.modTime) < activeWindow {
		sum.addSkippedSession(SkipActive, 1)
		return false, nil
	}

	// Recheck live routing state and team policy before reading and preparing segments.
	freshState, freshTeam, skipReason, reasonDetail, ok := recheckRouteTeam(opts, b.team.EnrollmentID, now)
	if !ok {
		return stopRouteWithIssue(b, remaining, skipReason, reasonDetail, report, sum), nil
	}
	sessionResolver := newCwdResolver(freshState, opts.AuthURL, opts.ServerKey, opts.ServerKeyTeam, opts.ServerKeyAuthURL, now)
	preparedPolicy := freshTeam.Policy

	report(ProgressEvent{
		Kind:         ProgressSessionStart,
		RouteLabel:   b.label,
		TeamID:       b.team.TeamID,
		SourceSystem: sp.file.SourceSystem,
		SessionID:    sp.file.SessionID,
		Index:        idx,
		Total:        total,
	})

	runSnap := runs.Read(opts.StateDir, sp.file.SessionID)
	subSnaps := map[string]runs.Snapshot{}
	lookupSnap := func(id string) runs.Snapshot {
		if id == sp.file.SessionID {
			return runSnap
		}
		if s, hit := subSnaps[id]; hit {
			return s
		}
		s := runs.Read(opts.StateDir, id)
		subSnaps[id] = s
		return s
	}

	// Re-read this session from disk, resolve every segment, then compute one
	// session-wide historical tool-reach Mixed cutoff across all segments and subagents.
	var resolvedSegs []resolvedSegment
	readErr := sp.source.ReadHistory(ctx, sp.file, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		target, ok, skipReason := sessionResolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
		resolvedSegs = append(resolvedSegs, resolvedSegment{seg: seg, target: target, ok: ok, skipReason: skipReason})
		return nil
	})
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) {
			sum.Interrupted = true
			return true, readErr
		}
		if errors.Is(readErr, fs.ErrNotExist) {
			sum.addSkippedSession(SkipMissingFile, 1)
		} else {
			sum.addSkippedSession(SkipUnreadable, 1)
		}
		return false, nil
	}

	// Re-stat immediately after ReadHistory so a turn appended while reading never leaves.
	afterReadSnap, stillExists := statHistoryFile(sp.file)
	if !stillExists {
		sum.addSkippedSession(SkipMissingFile, 1)
		return false, nil
	}
	if afterReadSnap != preSnap || nowFn().Sub(afterReadSnap.modTime) < activeWindow {
		sum.addSkippedSession(SkipActive, 1)
		return false, nil
	}

	var resumeMax time.Time
	if prev, ok := st.InProgress[sp.key]; ok && !prev.SourceModTime.IsZero() && preSnap.modTime.After(prev.SourceModTime) {
		resumeMax = prev.SourceModTime
	}

	reachMixedAt, hasReachMixed := sessionReachMixedAt(sessionResolver, resolvedSegs)
	var batches []*logspb.LogsData
	var keptSegs []agents.HistorySegment
	totalRecs := 0
	mixedDropped := 0
	for _, rs := range resolvedSegs {
		if !rs.ok || rs.target.Team.EnrollmentID != b.team.EnrollmentID {
			continue
		}
		prepared, n, dropped := prepareSegmentFiltered(rs.seg, opts.Filter, preparedPolicy, buildStampOptions(opts, rs.target, userIDFor), runSnap, lookupSnap, reachMixedAt, hasReachMixed, resumeMax)
		mixedDropped += dropped
		if n > 0 && prepared != nil {
			batches = append(batches, prepared)
			keptSegs = append(keptSegs, rs.seg)
			totalRecs += n
		}
	}

	if totalRecs == 0 {
		if st.IsInProgress(sp.key) {
			delete(st.InProgress, sp.key)
			if err := SaveState(opts.StateDir, *st); err != nil {
				return true, fmt.Errorf("save history state: %w", err)
			}
		}
		if mixedDropped > 0 {
			sum.addSkippedSession(SkipMixed, 1)
		} else {
			sum.addSkippedSession(SkipEmptyAfterFilter, 1)
		}
		return false, nil
	}

	chunks, chunkErr := ChunkLogs(batches, maxReqBytes)
	if chunkErr != nil {
		if perm, ok := errors.AsType[*PermanentError](chunkErr); ok {
			return false, markSessionFailed(opts.StateDir, st, b, sp, perm, nowFn(), report, sum)
		}
		return false, chunkErr
	}
	var totalBytes int64
	for _, ch := range chunks {
		totalBytes += int64(len(ch.Body))
	}

	// Record in_progress BEFORE the first request for this session.
	st.MarkInProgress(sp.file.SourceSystem, sp.file.SessionID, nowFn(), totalRecs, totalBytes, preSnap.modTime)
	if err := SaveState(opts.StateDir, *st); err != nil {
		return true, fmt.Errorf("save history state: %w", err)
	}

	lastCheckTeam := freshTeam
	lastOrgEpochs := orgEpochSnapshot(freshState.Teams, freshTeam.OrganizationID, freshTeam.AuthURL)
	beforeAttempt := func(attemptCtx context.Context) error {
		curNow := nowFn()
		snap, existsNow := statHistoryFile(sp.file)
		if !existsNow {
			return &preAttemptStopError{skipReason: SkipMissingFile, reason: "session transcript removed during upload", active: true}
		}
		if snap != preSnap || curNow.Sub(snap.modTime) < activeWindow {
			return &preAttemptStopError{skipReason: SkipActive, reason: "session transcript modified during upload", active: true}
		}
		checkState, checkTeam, checkSkip, checkReason, ok := recheckRouteTeam(opts, b.team.EnrollmentID, curNow)
		if !ok {
			return &preAttemptStopError{skipReason: checkSkip, reason: checkReason}
		}
		if checkState.StateID != freshState.StateID || checkState.ConsentGeneration != freshState.ConsentGeneration {
			return &preAttemptStopError{skipReason: SkipDeselected, reason: "routing selections or repository bindings changed during upload; re-run to apply"}
		}
		if !policyContentMatches(preparedPolicy, checkTeam.Policy) {
			return &preAttemptStopError{skipReason: SkipPolicyChanged, reason: "team content policy changed during upload; re-run to apply updated policy"}
		}
		// If any team's policy refreshed (Generation changed), re-verify that every kept
		// segment still routes to this enrollment (e.g. another org didn't just claim an
		// unbound repo that previously fell through to Personal).
		if !routing.SameGeneration(freshState, checkState) {
			recheckResolver := newCwdResolver(checkState, opts.AuthURL, opts.ServerKey, opts.ServerKeyTeam, opts.ServerKeyAuthURL, curNow)
			for _, seg := range keptSegs {
				target, stillOK, why := recheckResolver.resolve(attemptCtx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
				if !stillOK || target.Team.EnrollmentID != b.team.EnrollmentID {
					if why == "" {
						why = SkipPolicyChanged
					}
					return &preAttemptStopError{skipReason: why, reason: "routing policy changed during upload; re-run to apply"}
				}
			}
		}
		lastCheckTeam = checkTeam
		lastOrgEpochs = orgEpochSnapshot(checkState.Teams, checkTeam.OrganizationID, checkTeam.AuthURL)
		return nil
	}

	for _, ch := range chunks {
		sendErr := pacer.Send(ctx, SendRequest{
			Endpoint:      endpoint,
			TeamID:        b.team.TeamID,
			Account:       opts.Account,
			ServerKey:     serverKey,
			Body:          ch.Body,
			BeforeAttempt: beforeAttempt,
		})
		if sendErr == nil {
			continue
		}
		if errors.Is(sendErr, context.Canceled) {
			sum.Interrupted = true
			return true, sendErr
		}
		if preStop, ok := errors.AsType[*preAttemptStopError](sendErr); ok {
			if preStop.active {
				sum.addSkippedSession(preStop.skipReason, 1)
				return false, nil
			}
			return stopRouteWithIssue(b, remaining, preStop.skipReason, preStop.reason, report, sum), nil
		}
		if retired, ok := errors.AsType[*SignInRetiredError](sendErr); ok {
			return true, retired
		}
		if lost, ok := errors.AsType[*OrgAccessLostError](sendErr); ok {
			orgBlocked[b.team.OrganizationID] = lost.Error()
			if opts.OnOrgAccessLost != nil {
				opts.OnOrgAccessLost(lastCheckTeam.OrganizationID, lastCheckTeam.AuthURL, lastOrgEpochs, nowFn().UTC())
			}
			return stopRouteWithIssue(b, remaining, SkipOrgAccessLost, lost.Error(), report, sum), nil
		}
		if routeErr, ok := errors.AsType[*RouteError](sendErr); ok {
			// Leave in_progress so fixing credentials or configuration lets the next run resume.
			return stopRouteWithIssue(b, remaining, SkipSendFailed, routeErr.Error(), report, sum), nil
		}
		if perm, ok := errors.AsType[*PermanentError](sendErr); ok {
			return false, markSessionFailed(opts.StateDir, st, b, sp, perm, nowFn(), report, sum)
		}
		// Transient error after retries: leave in_progress so resume retries it first.
		return stopRouteWithIssue(b, remaining, SkipSendFailed, sendErr.Error(), report, sum), nil
	}

	// Re-stat right before marking done: if the user resumed the session while we were
	// uploading it, keep it in_progress so later turns are never skipped.
	postSnap, stillHere := statHistoryFile(sp.file)
	if !stillHere {
		sum.addSkippedSession(SkipMissingFile, 1)
		return false, nil
	}
	if postSnap != preSnap || nowFn().Sub(postSnap.modTime) < activeWindow {
		sum.addSkippedSession(SkipActive, 1)
		return false, nil
	}

	// Move to done ONLY after every request succeeded with zero rejected records.
	st.MarkDone(sp.file.SourceSystem, sp.file.SessionID, nowFn(), totalRecs, totalBytes)
	if err := SaveState(opts.StateDir, *st); err != nil {
		return true, fmt.Errorf("save history state: %w", err)
	}
	sum.SentSessions++
	sum.SentRecords += totalRecs
	sum.SentBytes += totalBytes
	report(ProgressEvent{
		Kind:         ProgressSessionDone,
		RouteLabel:   b.label,
		TeamID:       b.team.TeamID,
		SourceSystem: sp.file.SourceSystem,
		SessionID:    sp.file.SessionID,
		Index:        idx,
		Total:        total,
		Records:      totalRecs,
		Bytes:        totalBytes,
	})
	return false, nil
}

func stopRouteWithIssue(b *routeBucket, remaining int, skipReason, reason string, report func(ProgressEvent), sum *Summary) bool {
	sum.addSkippedSession(skipReason, remaining)
	sum.TeamIssues = append(sum.TeamIssues, TeamIssue{
		EnrollmentID: b.team.EnrollmentID,
		TeamID:       b.team.TeamID,
		TeamLabel:    b.label,
		Skipped:      remaining,
		Reason:       reason,
	})
	report(ProgressEvent{Kind: ProgressRouteBlocked, RouteLabel: b.label, TeamID: b.team.TeamID, Reason: reason})
	return true
}

func markSessionFailed(
	stateDir string,
	st *RouteState,
	b *routeBucket,
	sp *routeSessionPlan,
	perm *PermanentError,
	at time.Time,
	report func(ProgressEvent),
	sum *Summary,
) error {
	st.MarkFailed(sp.file.SourceSystem, sp.file.SessionID, at, perm.Reason, perm.Rejected)
	if err := SaveState(stateDir, *st); err != nil {
		return fmt.Errorf("save history state: %w", err)
	}
	sum.Failed = append(sum.Failed, FailedSession{
		EnrollmentID:    b.team.EnrollmentID,
		TeamID:          b.team.TeamID,
		TeamLabel:       b.label,
		SourceSystem:    sp.file.SourceSystem,
		SessionID:       sp.file.SessionID,
		Reason:          perm.Reason,
		RejectedRecords: perm.Rejected,
		FailedAt:        at.UTC(),
	})
	report(ProgressEvent{
		Kind:         ProgressSessionFailed,
		RouteLabel:   b.label,
		TeamID:       b.team.TeamID,
		SourceSystem: sp.file.SourceSystem,
		SessionID:    sp.file.SessionID,
		Reason:       perm.Reason,
	})
	return nil
}

func resolveRouteServerKey(opts Options, b *routeBucket, totalSkip int, report func(ProgressEvent), sum *Summary) (string, bool) {
	if !opts.ServerKey || opts.ServerKeyFor == nil {
		return "", true
	}
	key, err := opts.ServerKeyFor(b.team.TeamID)
	if err == nil && key != "" {
		return key, true
	}
	reason := "no server key available for this team"
	if err != nil {
		reason = err.Error()
	}
	stopRouteWithIssue(b, totalSkip, SkipSendFailed, reason, report, sum)
	return "", false
}

func filterHeldCandidates(
	ctx context.Context,
	opts Options,
	freshState routing.State,
	b *routeBucket,
	candidateQueue []*routeSessionPlan,
	nowFn func() time.Time,
	report func(ProgressEvent),
	orgBlocked map[string]string,
	sum *Summary,
) ([]*routeSessionPlan, error) {
	ids := make([]string, 0, len(candidateQueue))
	for _, sp := range candidateQueue {
		ids = append(ids, sp.file.SessionID)
	}
	report(ProgressEvent{Kind: ProgressHoldings, RouteLabel: b.label, TeamID: b.team.TeamID, Total: len(ids)})
	if opts.Holdings == nil {
		stopRouteWithIssue(b, len(candidateQueue), SkipHoldingsFailed, "holdings check is unavailable", report, sum)
		return nil, nil
	}
	orgEpochs := orgEpochSnapshot(freshState.Teams, b.team.OrganizationID, b.team.AuthURL)
	held, err := opts.Holdings.Check(ctx, b.team, ids)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			sum.Interrupted = true
			return nil, err
		}
		if errors.Is(err, auth.ErrSignInRetired) {
			return nil, &SignInRetiredError{Detail: err.Error()}
		}
		if apiErr, ok := errors.AsType[*api.Error](err); ok {
			if apiErr.SignInRetired() {
				return nil, &SignInRetiredError{Detail: apiErr.Error()}
			}
			if apiErr.OrgNotAccessible() {
				orgBlocked[b.team.OrganizationID] = apiErr.Error()
				if opts.OnOrgAccessLost != nil {
					opts.OnOrgAccessLost(b.team.OrganizationID, b.team.AuthURL, orgEpochs, nowFn().UTC())
				}
				stopRouteWithIssue(b, len(candidateQueue), SkipOrgAccessLost, apiErr.Error(), report, sum)
				return nil, nil
			}
		}
		stopRouteWithIssue(b, len(candidateQueue), SkipHoldingsFailed, fmt.Sprintf("holdings check failed: %v", err), report, sum)
		return nil, nil
	}

	var uploadQueue []*routeSessionPlan
	for _, sp := range candidateQueue {
		if held.Contains(sp.file.SourceSystem, sp.file.SessionID) {
			sum.addSkippedSession(SkipAlreadyHeld, 1)
			continue
		}
		uploadQueue = append(uploadQueue, sp)
	}
	return uploadQueue, nil
}
