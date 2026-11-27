package history

import (
	"slices"
	"time"
)

// Canonical skip reasons reported in Summary.SkippedSessions and Summary.SkippedSegments.
const (
	SkipActive            = "active"
	SkipOutsideSince      = "outside_since"
	SkipAlreadyDone       = "already_done"
	SkipAlreadyHeld       = "already_held"
	SkipFailedPreviously  = "failed_previously"
	SkipHoldingsFailed    = "holdings_check_failed"
	SkipOrgAccessLost     = "org_access_lost"
	SkipSendFailed        = "send_failed"
	SkipDeselected        = "deselected"
	SkipPolicyChanged     = "policy_changed"
	SkipMixed             = "mixed"
	SkipNoCwd             = "no_cwd"
	SkipMissingCwd        = "missing_cwd"
	SkipOriginMismatch    = "origin_mismatch"
	SkipMissingFile       = "missing_file"
	SkipUnreadable        = "unreadable"
	SkipUnroutable        = "unroutable"
	SkipNotCollected      = "not_collected"
	SkipWrongEnvironment  = "wrong_environment"
	SkipEmptyAfterFilter  = "empty_after_filter"
	SkipRecordTooLarge    = "record_too_large"
	SkipTeamBlockedPrefix = "team_blocked:"
)

// DefaultActiveWindow is how recently a session's files may have changed for an upload to
// leave it out as still in progress, unless Options.ActiveWindow says otherwise.
const DefaultActiveWindow = time.Hour

// ProgressKind identifies the phase of a ProgressEvent callback.
type ProgressKind string

// Progress event kinds delivered to Options.Progress.
const (
	// ProgressStart fires once the upload lock is held and resume state is loaded, just
	// before discovery begins.
	ProgressStart    ProgressKind = "start"
	ProgressDiscover ProgressKind = "discover"
	ProgressHoldings ProgressKind = "holdings"
	// ProgressPlan comes once, after every team's holdings check and before the first
	// session is sent, with what is left to send in Plan.
	ProgressPlan          ProgressKind = "plan"
	ProgressSessionStart  ProgressKind = "session_start"
	ProgressSessionDone   ProgressKind = "session_done"
	ProgressSessionFailed ProgressKind = "session_failed"
	// ProgressSessionSkipped is one session left out, its skip reason in SkipReason.
	ProgressSessionSkipped ProgressKind = "session_skipped"
	// ProgressRouteBlocked is a team whose remaining Skipped sessions wait for another run.
	ProgressRouteBlocked ProgressKind = "route_blocked"
)

// Phases in which ProgressRouteBlocked can be emitted. Only plan and upload blocks are
// counted in Summary.TeamIssues; discovery blocks are counted in Summary.SkippedSegments.
const (
	BlockPhaseDiscovery = "discovery"
	BlockPhasePlan      = "plan"
	BlockPhaseUpload    = "upload"
)

// ProgressEvent reports live upload progress to the CLI.
type ProgressEvent struct {
	Kind         ProgressKind
	RouteLabel   string
	EnrollmentID string
	TeamID       string
	SourceSystem string
	SessionID    string
	// Index and Total number a session within its team's resumed or new sessions.
	Index int
	Total int
	// Overall numbers a session across every team's plan, of OverallTotal. Every session
	// event is for a planned session.
	Overall      int
	OverallTotal int
	Records      int
	Bytes        int64
	// SourceBytes is the size of the session's files on disk, as the plan counted them.
	SourceBytes int64
	Reason      string
	// SkipReason is one of the Skip* reasons, for a skipped session or a blocked team.
	SkipReason string
	// Phase is BlockPhaseDiscovery, BlockPhasePlan, or BlockPhaseUpload on ProgressRouteBlocked.
	Phase string
	// Skipped is how many sessions a blocked team left out.
	Skipped int
	// Skips counts the sessions discovery left out, by skip reason.
	Skips map[string]int
	Plan  *UploadPlan
}

// UploadPlan is what a run has left to send, in all and per team: in a dry run, before any
// server is asked what it holds.
type UploadPlan struct {
	HoldingsChecked bool  `json:"holdings_checked"`
	Sessions        int   `json:"sessions"`
	ResumeSessions  int   `json:"resume_sessions"`
	Records         int   `json:"records"`
	SourceBytes     int64 `json:"source_bytes"`
	// EstimatedBytes is measured in a dry run only.
	EstimatedBytes int64 `json:"estimated_bytes,omitempty"`
	// SkippedSessions counts the sessions planning left out (already_done,
	// failed_previously, already_held), by skip reason; none of them is in Sessions.
	SkippedSessions map[string]int `json:"skipped_sessions"`
	Teams           []PlanTeam     `json:"teams"`
}

// PlanTeam is one team's share of an UploadPlan.
type PlanTeam struct {
	EnrollmentID   string `json:"enrollment_id"`
	TeamID         string `json:"team_id"`
	TeamLabel      string `json:"team_label"`
	Sessions       int    `json:"sessions"`
	ResumeSessions int    `json:"resume_sessions"`
	Records        int    `json:"records"`
	SourceBytes    int64  `json:"source_bytes"`
	EstimatedBytes int64  `json:"estimated_bytes,omitempty"`
	// SkippedSessions is this team's share of UploadPlan.SkippedSessions.
	SkippedSessions map[string]int `json:"skipped_sessions"`
}

func sessionEvent(kind ProgressKind, b *routeBucket, sp *routeSessionPlan, skipReason string) ProgressEvent {
	return ProgressEvent{
		Kind:         kind,
		RouteLabel:   b.label,
		EnrollmentID: b.team.EnrollmentID,
		TeamID:       b.team.TeamID,
		SourceSystem: sp.file.SourceSystem,
		SessionID:    sp.file.SessionID,
		Overall:      sp.overall,
		SourceBytes:  sp.sourceBytes,
		SkipReason:   skipReason,
	}
}

// RoutePlan summarizes what a run (or --dry-run) plans to upload to one destination team.
type RoutePlan struct {
	EnrollmentID   string `json:"enrollment_id"`
	TeamID         string `json:"team_id"`
	TeamLabel      string `json:"team_label"`
	Sessions       int    `json:"sessions"`
	ResumeSessions int    `json:"resume_sessions,omitempty"`
	Records        int    `json:"records"`
	EstimatedBytes int64  `json:"estimated_bytes"`
}

// FailedSession names one permanently failed session and why it failed.
type FailedSession struct {
	EnrollmentID    string    `json:"enrollment_id"`
	TeamID          string    `json:"team_id"`
	TeamLabel       string    `json:"team_label"`
	SourceSystem    string    `json:"source_system"`
	SessionID       string    `json:"session_id"`
	Reason          string    `json:"reason"`
	RejectedRecords int64     `json:"rejected_records,omitempty"`
	Previously      bool      `json:"previously,omitempty"`
	FailedAt        time.Time `json:"failed_at"`
}

// TeamIssue records a destination-level failure (such as a failed holdings check or lost
// org access) that caused sessions for that team to be skipped fail-closed.
type TeamIssue struct {
	EnrollmentID string `json:"enrollment_id"`
	TeamID       string `json:"team_id"`
	TeamLabel    string `json:"team_label"`
	Skipped      int    `json:"skipped"`
	Reason       string `json:"reason"`
}

// Summary is the complete structured outcome of one Upload run.
type Summary struct {
	DryRun          bool           `json:"dry_run,omitempty"`
	Interrupted     bool           `json:"interrupted,omitempty"`
	DiscoveredFiles int            `json:"discovered_files"`
	SentSessions    int            `json:"sent_sessions"`
	SentRecords     int            `json:"sent_records"`
	SentBytes       int64          `json:"sent_bytes"`
	Planned         []RoutePlan    `json:"planned,omitempty"`
	SkippedSessions map[string]int `json:"skipped_sessions,omitempty"`
	SkippedSegments map[string]int `json:"skipped_segments,omitempty"`
	// WithheldRecords counts records not sent (or, in a dry run, not planned) because they
	// fall at or after their session's Mixed cutoff, including in sessions partly sent.
	WithheldRecords int             `json:"withheld_records,omitempty"`
	Failed          []FailedSession `json:"failed,omitempty"`
	TeamIssues      []TeamIssue     `json:"team_issues,omitempty"`
	// Unreadable names up to three transcripts or directories discovery could not read;
	// sessions that may depend on them were skipped as unreadable.
	Unreadable []string `json:"unreadable,omitempty"`
}

func newSummary(dryRun bool) Summary {
	return Summary{
		DryRun:          dryRun,
		SkippedSessions: map[string]int{},
		SkippedSegments: map[string]int{},
	}
}

func (s *Summary) addSkippedSession(reason string, n int) {
	if n <= 0 {
		return
	}
	if s.SkippedSessions == nil {
		s.SkippedSessions = map[string]int{}
	}
	s.SkippedSessions[reason] += n
}

func (s *Summary) addSkippedSegment(reason string, n int) {
	if n <= 0 {
		return
	}
	if s.SkippedSegments == nil {
		s.SkippedSegments = map[string]int{}
	}
	s.SkippedSegments[reason] += n
}

// maxUnreadablePaths caps Summary.Unreadable so one broken directory cannot flood output.
const maxUnreadablePaths = 3

func (s *Summary) addUnreadable(path string) {
	if path == "" || len(s.Unreadable) >= maxUnreadablePaths || slices.Contains(s.Unreadable, path) {
		return
	}
	s.Unreadable = append(s.Unreadable, path)
}

// SortedSkipKeys returns map keys sorted for deterministic output.
func SortedSkipKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v > 0 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
