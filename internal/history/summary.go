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

// ProgressKind identifies the phase of a ProgressEvent callback.
type ProgressKind string

// Progress event kinds delivered to Options.Progress.
const (
	ProgressDiscover      ProgressKind = "discover"
	ProgressHoldings      ProgressKind = "holdings"
	ProgressSessionStart  ProgressKind = "session_start"
	ProgressSessionDone   ProgressKind = "session_done"
	ProgressSessionFailed ProgressKind = "session_failed"
	ProgressRouteBlocked  ProgressKind = "route_blocked"
)

// ProgressEvent reports live upload progress to the CLI.
type ProgressEvent struct {
	Kind         ProgressKind
	RouteLabel   string
	TeamID       string
	SourceSystem string
	SessionID    string
	Index        int
	Total        int
	Records      int
	Bytes        int64
	Reason       string
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
