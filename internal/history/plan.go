package history

import (
	"context"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"
)

// routeWork is one team's share of the plan: the sessions it resumes, sent first and
// without a holdings check, and those left after the holdings check.
type routeWork struct {
	b       *routeBucket
	st      RouteState
	resume  []*routeSessionPlan
	upload  []*routeSessionPlan
	skipped map[string]int // sessions planning left out, by skip reason
	key     string         // the team's server key, on a server-key profile
}

// planRoute sorts one team's sessions into what was sent or failed before, what resumes
// and what is new, and, outside a dry run, asks the server which of the new ones it holds.
func planRoute(
	ctx context.Context,
	opts Options,
	existingStates map[string]RouteState,
	b *routeBucket,
	nowFn func() time.Time,
	report func(ProgressEvent),
	orgBlocked map[string]string,
	sum *Summary,
) (*routeWork, error) {
	eid := b.team.EnrollmentID
	st, ok := existingStates[eid]
	if !ok {
		var err error
		st, _, err = LoadState(opts.StateDir, opts.Account, opts.Profile, eid, b.team.TeamID)
		if err != nil {
			return nil, err
		}
	}
	w := &routeWork{b: b, st: st, skipped: map[string]int{}}
	// Sessions left out while planning are counted on the plan, not reported one by one: a
	// machine that uploaded thousands before would otherwise repeat them on every run.
	skipped := func(reason string) {
		sum.addSkippedSession(reason, 1)
		w.skipped[reason]++
	}

	var candidateQueue []*routeSessionPlan
	for _, sp := range sortedPlans(b.sessions) {
		if st.IsDone(sp.key) {
			skipped(SkipAlreadyDone)
			continue
		}
		if frec, failed := st.IsFailed(sp.key); failed && opts.RetryFailed {
			w.resume = append(w.resume, sp) // no holdings check: part may be held already
			continue
		} else if failed {
			skipped(SkipFailedPreviously)
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
			w.resume = append(w.resume, sp)
		} else {
			candidateQueue = append(candidateQueue, sp)
		}
	}

	rp := RoutePlan{
		EnrollmentID:   eid,
		TeamID:         b.team.TeamID,
		TeamLabel:      b.label,
		Sessions:       len(w.resume) + len(candidateQueue),
		ResumeSessions: len(w.resume),
	}
	for _, sp := range w.resume {
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
		w.upload = candidateQueue
		return w, nil
	}
	// An organization that refused access for an earlier team refuses this one too: it
	// stops whole, and its server is not asked.
	if reason, blocked := orgBlocked[b.team.OrganizationID]; blocked {
		stopRouteWithIssue(b, len(w.resume)+len(candidateQueue), SkipOrgAccessLost, reason, report, sum)
		w.resume = nil
		return w, nil
	}
	// A server-key profile without this team's key can neither ask nor send: the team
	// stops whole, before its server is asked.
	key, keyOK := resolveRouteServerKey(opts, b, len(w.resume)+len(candidateQueue), report, sum)
	if !keyOK {
		w.resume = nil
		return w, nil
	}
	w.key = key
	if len(candidateQueue) == 0 {
		return w, nil
	}
	// A team that cannot be sent to now stops whole, its resumed sessions with it, as the
	// first resumed session's own recheck would stop it.
	freshState, _, checkSkip, checkReason, routeOK := recheckRouteTeam(opts, eid, nowFn())
	if !routeOK {
		stopRouteWithIssue(b, len(w.resume)+len(candidateQueue), checkSkip, checkReason, report, sum)
		w.resume = nil
		return w, nil
	}
	upload, held, err := filterHeldCandidates(ctx, opts, freshState, b, candidateQueue, len(w.resume), nowFn, report, orgBlocked, sum)
	if err != nil {
		return nil, err
	}
	if held > 0 {
		w.skipped[SkipAlreadyHeld] += held
	}
	if _, blocked := orgBlocked[b.team.OrganizationID]; blocked {
		w.resume = nil // counted in the team's org_access_lost issue
	}
	w.upload = upload
	return w, nil
}

// buildUploadPlan totals what is left to send to every team, numbering the sessions in the
// order they will be sent. The sizes are each session's own files on disk, read now; a
// session whose parts go to two teams counts under each.
func buildUploadPlan(works []*routeWork, holdingsChecked bool) UploadPlan {
	plan := UploadPlan{HoldingsChecked: holdingsChecked, Teams: []PlanTeam{}, SkippedSessions: map[string]int{}}
	for _, w := range works {
		team := PlanTeam{EnrollmentID: w.b.team.EnrollmentID, TeamID: w.b.team.TeamID, TeamLabel: w.b.label, ResumeSessions: len(w.resume), SkippedSessions: map[string]int{}}
		for reason, n := range w.skipped {
			if n > 0 {
				team.SkippedSessions[reason] += n
				plan.SkippedSessions[reason] += n
			}
		}
		for _, sp := range slices.Concat(w.resume, w.upload) {
			// The session's own files: a subagent's transcript is a session of its own.
			if snap, reason := statHistoryFile(sp.file); reason == "" {
				sp.sourceBytes = snap.size
			}
			plan.Sessions++
			sp.overall = plan.Sessions
			team.Sessions++
			team.Records += sp.records
			team.SourceBytes += sp.sourceBytes
			team.EstimatedBytes += sp.estimatedBytes
		}
		if team.Sessions == 0 && len(team.SkippedSessions) == 0 {
			continue
		}
		plan.ResumeSessions += team.ResumeSessions
		plan.Records += team.Records
		plan.SourceBytes += team.SourceBytes
		plan.EstimatedBytes += team.EstimatedBytes
		plan.Teams = append(plan.Teams, team)
	}
	return plan
}

// uploadRoute sends one team's planned sessions: the resumed ones first, then the new ones
// its server did not hold.
func uploadRoute(
	ctx context.Context,
	opts Options,
	w *routeWork,
	activeWindow time.Duration,
	maxReqBytes int,
	userIDFor func(routing.Team, string) string,
	nowFn func() time.Time,
	report func(ProgressEvent),
	orgBlocked map[string]string,
	reachCache map[string]familyVerdict,
	sum *Summary,
) error {
	b := w.b
	total := len(w.resume) + len(w.upload)
	if total == 0 {
		return nil
	}
	if reason, blocked := orgBlocked[b.team.OrganizationID]; blocked {
		stopRouteWithIssue(b, total, SkipOrgAccessLost, reason, report, sum)
		return nil
	}

	pacer := opts.Transport.NewRoutePacer()
	endpoint := ""
	if opts.EndpointFor != nil {
		endpoint = opts.EndpointFor(b.team.TeamID)
	}
	serverKey := w.key

	for i, sp := range w.resume {
		stopRoute, retErr := uploadOneSession(ctx, opts, &w.st, b, sp, pacer, endpoint, serverKey, activeWindow, maxReqBytes, i+1, len(w.resume), len(w.upload), userIDFor, nowFn, report, sum, orgBlocked, reachCache)
		if retErr != nil {
			return retErr
		}
		if stopRoute {
			return nil
		}
	}
	for i, sp := range w.upload {
		stopRoute, retErr := uploadOneSession(ctx, opts, &w.st, b, sp, pacer, endpoint, serverKey, activeWindow, maxReqBytes, i+1, len(w.upload), 0, userIDFor, nowFn, report, sum, orgBlocked, reachCache)
		if retErr != nil {
			return retErr
		}
		if stopRoute {
			return nil
		}
	}
	return nil
}
