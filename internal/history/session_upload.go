package history

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// maxConsecutiveRejections is how many sessions in a row the server may reject on a route
// before the rest of that route waits for the next run.
const maxConsecutiveRejections = 5

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
	reachCache map[string]familyVerdict,
) (stopRoute bool, fatalErr error) {
	if err := ctx.Err(); err != nil {
		sum.Interrupted = true
		return true, err
	}
	remaining := max(1, total-idx+1) + max(0, extraRemaining)
	skipped := func(reason string) {
		sum.addSkippedSession(reason, 1)
		report(sessionEvent(ProgressSessionSkipped, b, sp, reason))
	}

	// Re-stat transcript and spawn-family files right before reading so a session resumed
	// while earlier routes were draining is never uploaded mid-write or marked done early.
	preSnap, statSkip := statSessionFiles(sp.file, sp.companions)
	if statSkip != "" {
		skipped(statSkip)
		return false, nil
	}
	now := nowFn()
	if changedWithin(now, preSnap.modTime, activeWindow) {
		skipped(SkipActive)
		return false, nil
	}

	// Recheck live routing state and team policy before reading and preparing segments.
	freshState, freshTeam, skipReason, reasonDetail, ok := recheckRouteTeam(opts, b.team.EnrollmentID, now)
	if !ok {
		return stopRouteWithIssue(b, remaining, skipReason, reasonDetail, report, sum), nil
	}
	sessionResolver := newCwdResolver(freshState, opts.AuthURL, opts.ServerKey, opts.ServerKeyTeam, opts.ServerKeyAuthURL, now)
	preparedPolicy := freshTeam.Policy

	start := sessionEvent(ProgressSessionStart, b, sp, "")
	start.Index, start.Total = idx, total
	report(start)

	runSnap, lookupSnap := newRunSnapLookup(opts.StateDir, sp.file.SessionID)

	// Pass 1 reads the spawn family's routing and tool reach, without keeping log records.
	reachMixedAt, hasReachMixed, readErr := familyCutoff(ctx, opts, sp.source, sessionResolver, sp.file, sp.companions, sp.missingParent, preSnap, reachCache)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) {
			sum.Interrupted = true
			return true, readErr
		}
		if errors.Is(readErr, fs.ErrNotExist) {
			skipped(SkipMissingFile)
		} else {
			skipped(SkipUnreadable)
		}
		return false, nil
	}

	// Re-stat immediately after Pass 1 so a turn appended while reading never leaves.
	afterReadSnap, statSkip := statSessionFiles(sp.file, sp.companions)
	if statSkip != "" {
		skipped(statSkip)
		return false, nil
	}
	if afterReadSnap != preSnap || changedWithin(nowFn(), afterReadSnap.modTime, activeWindow) {
		skipped(SkipActive)
		return false, nil
	}

	var resumeMax time.Time
	if prev, ok := st.InProgress[sp.key]; ok && !prev.SourceModTime.IsZero() && preSnap.modTime.After(prev.SourceModTime) {
		resumeMax = prev.SourceModTime
	}

	lastCheckTeam := freshTeam
	lastOrgEpochs := orgEpochSnapshot(freshState.Teams, freshTeam.OrganizationID, freshTeam.AuthURL)
	beforeAttempt := func(context.Context) error {
		curNow := nowFn()
		snap, statSkip := statSessionFiles(sp.file, sp.companions)
		if statSkip != "" {
			return &preAttemptStopError{skipReason: statSkip, reason: "session transcript changed or removed during upload", active: true}
		}
		if snap != preSnap || changedWithin(curNow, snap.modTime, activeWindow) {
			return &preAttemptStopError{skipReason: SkipActive, reason: "session transcript modified during upload", active: true}
		}
		checkState, checkTeam, checkSkip, checkReason, ok := recheckRouteTeam(opts, b.team.EnrollmentID, curNow)
		if !ok {
			return &preAttemptStopError{skipReason: checkSkip, reason: checkReason}
		}
		if checkState.StateID != freshState.StateID || checkState.ConsentGeneration != freshState.ConsentGeneration {
			return &preAttemptStopError{skipReason: SkipDeselected, reason: "team selections changed during upload; re-run to apply"}
		}
		if !routingPoliciesMatch(freshState, checkState, curNow) {
			return &preAttemptStopError{skipReason: SkipPolicyChanged, reason: "routing or content policy changed during upload; re-run to apply updated policy"}
		}
		lastCheckTeam = checkTeam
		lastOrgEpochs = orgEpochSnapshot(checkState.Teams, checkTeam.OrganizationID, checkTeam.AuthURL)
		return nil
	}

	// Pass 2 (bounded memory): stream segments into one bounded <= maxReqBytes OTLP chunk
	// at a time, recording in_progress immediately before the first POST.
	markedInProgress := false
	var stateSaveErr error
	totalRecs := 0
	var totalBytes int64
	mixedDropped := 0
	chunker := NewChunkWriter(maxReqBytes, func(ch Chunk) error {
		if !markedInProgress {
			if err := beforeAttempt(ctx); err != nil {
				return err
			}
			st.MarkInProgress(sp.file.SourceSystem, sp.file.SessionID, nowFn(), sp.records, sp.estimatedBytes, preSnap.modTime)
			if err := SaveState(opts.StateDir, *st); err != nil {
				stateSaveErr = fmt.Errorf("save history state: %w", err)
				return stateSaveErr
			}
			markedInProgress = true
		}
		if err := pacer.Send(ctx, SendRequest{
			Endpoint:      endpoint,
			TeamID:        b.team.TeamID,
			Account:       opts.Account,
			ServerKey:     serverKey,
			Body:          ch.Body,
			BeforeAttempt: beforeAttempt,
		}); err != nil {
			return err
		}
		totalBytes += int64(len(ch.Body))
		return nil
	})

	streamErr := sp.source.ReadHistory(ctx, sp.file, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		if seg.RecordCount == 0 {
			return nil
		}
		target, ok, _ := sessionResolver.resolve(ctx, seg.Cwd, seg.FallbackRepoURL, seg.FallbackBranch)
		if !ok || target.Team.EnrollmentID != b.team.EnrollmentID {
			return nil
		}
		prepared, n, dropped := prepareSegmentFiltered(seg, opts.Filter, preparedPolicy, buildStampOptions(opts, target, userIDFor), runSnap, lookupSnap, reachMixedAt, hasReachMixed, resumeMax)
		mixedDropped += dropped
		if n == 0 || prepared == nil {
			return nil
		}
		totalRecs += n
		return chunker.Add(prepared)
	})
	if streamErr == nil {
		streamErr = chunker.Flush()
	}
	if streamErr != nil {
		if stateSaveErr != nil {
			return true, stateSaveErr
		}
		if errors.Is(streamErr, context.Canceled) {
			sum.Interrupted = true
			return true, streamErr
		}
		if preStop, ok := errors.AsType[*preAttemptStopError](streamErr); ok {
			if preStop.active {
				skipped(preStop.skipReason)
				return false, nil
			}
			return stopRouteWithIssue(b, remaining, preStop.skipReason, preStop.reason, report, sum), nil
		}
		if retired, ok := errors.AsType[*SignInRetiredError](streamErr); ok {
			return true, retired
		}
		if lost, ok := errors.AsType[*OrgAccessLostError](streamErr); ok {
			orgBlocked[b.team.OrganizationID] = lost.Error()
			if opts.OnOrgAccessLost != nil {
				opts.OnOrgAccessLost(lastCheckTeam.OrganizationID, lastCheckTeam.AuthURL, lastOrgEpochs, nowFn().UTC())
			}
			return stopRouteWithIssue(b, remaining, SkipOrgAccessLost, lost.Error(), report, sum), nil
		}
		if routeErr, ok := errors.AsType[*RouteError](streamErr); ok {
			return stopRouteWithIssue(b, remaining, SkipSendFailed, routeErr.Error(), report, sum), nil
		}
		if perm, ok := errors.AsType[*PermanentError](streamErr); ok {
			if err := markSessionFailed(opts.StateDir, st, b, sp, perm, nowFn(), report, sum); err != nil || perm.Status == 0 {
				return err != nil, err
			}
			// A server that rejects session after session will likely reject the rest too.
			if b.rejected++; b.rejected >= maxConsecutiveRejections {
				reason := fmt.Sprintf("the server rejected %d sessions in a row (last: %s); fix the cause and run `terma history upload --retry-failed`", b.rejected, perm.Reason)
				return stopRouteWithIssue(b, remaining-1, SkipSendFailed, reason, report, sum), nil
			}
			return false, nil
		}
		if !markedInProgress {
			if errors.Is(streamErr, fs.ErrNotExist) {
				skipped(SkipMissingFile)
			} else {
				skipped(SkipUnreadable)
			}
			return false, nil
		}
		return stopRouteWithIssue(b, remaining, SkipSendFailed, streamErr.Error(), report, sum), nil
	}

	sum.WithheldRecords += mixedDropped
	if totalRecs == 0 {
		if _, failed := st.IsFailed(sp.key); failed || st.IsInProgress(sp.key) {
			delete(st.InProgress, sp.key)
			delete(st.Failed, sp.key)
			if err := SaveState(opts.StateDir, *st); err != nil {
				return true, fmt.Errorf("save history state: %w", err)
			}
		}
		if mixedDropped > 0 {
			skipped(SkipMixed)
		} else {
			skipped(SkipEmptyAfterFilter)
		}
		return false, nil
	}

	// Re-stat right before marking done: if the user resumed the session while we were
	// uploading it, keep it in_progress so later turns are never skipped.
	postSnap, statSkip := statSessionFiles(sp.file, sp.companions)
	if statSkip != "" {
		skipped(statSkip)
		return false, nil
	}
	if postSnap != preSnap || changedWithin(nowFn(), postSnap.modTime, activeWindow) {
		skipped(SkipActive)
		return false, nil
	}

	// Move to done ONLY after every request succeeded with zero rejected records.
	st.MarkDone(sp.file.SourceSystem, sp.file.SessionID, nowFn(), totalRecs, totalBytes)
	if err := SaveState(opts.StateDir, *st); err != nil {
		return true, fmt.Errorf("save history state: %w", err)
	}
	b.rejected = 0
	sum.SentSessions++
	sum.SentRecords += totalRecs
	sum.SentBytes += totalBytes
	done := sessionEvent(ProgressSessionDone, b, sp, "")
	done.Index, done.Total, done.Records, done.Bytes = idx, total, totalRecs, totalBytes
	report(done)
	return false, nil
}

// changedWithin reports whether files changed at mod are still within window of now. A
// zero window counts nothing as changed within it, a modification time in the future
// included.
func changedWithin(now, mod time.Time, window time.Duration) bool {
	return window > 0 && now.Sub(mod) < window
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
	report(ProgressEvent{
		Kind:         ProgressRouteBlocked,
		RouteLabel:   b.label,
		EnrollmentID: b.team.EnrollmentID,
		TeamID:       b.team.TeamID,
		Reason:       reason,
		SkipReason:   skipReason,
		Skipped:      remaining,
	})
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
	failed := sessionEvent(ProgressSessionFailed, b, sp, "")
	failed.Reason = perm.Reason
	report(failed)
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
	resumed int,
	nowFn func() time.Time,
	report func(ProgressEvent),
	orgBlocked map[string]string,
	sum *Summary,
) (upload []*routeSessionPlan, held int, err error) {
	ids := make([]string, 0, len(candidateQueue))
	for _, sp := range candidateQueue {
		ids = append(ids, sp.file.SessionID)
	}
	report(ProgressEvent{Kind: ProgressHoldings, RouteLabel: b.label, EnrollmentID: b.team.EnrollmentID, TeamID: b.team.TeamID, Total: len(ids)})
	if opts.Holdings == nil {
		stopRouteWithIssue(b, len(candidateQueue), SkipHoldingsFailed, "holdings check is unavailable", report, sum)
		return nil, 0, nil
	}
	orgEpochs := orgEpochSnapshot(freshState.Teams, b.team.OrganizationID, b.team.AuthURL)
	heldSet, err := opts.Holdings.Check(ctx, b.team, ids)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			sum.Interrupted = true
			return nil, 0, err
		}
		if errors.Is(err, auth.ErrSignInRetired) {
			return nil, 0, &SignInRetiredError{Detail: err.Error()}
		}
		if apiErr, ok := errors.AsType[*api.Error](err); ok {
			if apiErr.SignInRetired() {
				return nil, 0, &SignInRetiredError{Detail: apiErr.Error()}
			}
			if apiErr.OrgNotAccessible() {
				orgBlocked[b.team.OrganizationID] = apiErr.Error()
				if opts.OnOrgAccessLost != nil {
					opts.OnOrgAccessLost(b.team.OrganizationID, b.team.AuthURL, orgEpochs, nowFn().UTC())
				}
				// The team's resumed sessions would be refused too.
				stopRouteWithIssue(b, resumed+len(candidateQueue), SkipOrgAccessLost, apiErr.Error(), report, sum)
				return nil, 0, nil
			}
		}
		stopRouteWithIssue(b, len(candidateQueue), SkipHoldingsFailed, fmt.Sprintf("holdings check failed: %v", err), report, sum)
		return nil, 0, nil
	}

	for _, sp := range candidateQueue {
		if heldSet.Contains(sp.file.SourceSystem, sp.file.SessionID) {
			sum.addSkippedSession(SkipAlreadyHeld, 1)
			held++
			continue
		}
		upload = append(upload, sp)
	}
	return upload, held, nil
}
