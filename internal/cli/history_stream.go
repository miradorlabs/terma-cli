package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/history"
)

// The --json-stream contract of `terma history upload`, read by Terma Desktop. Every line
// on stdout is one JSON object whose "type" is one of the constants below; field names
// never change, and a new field or type may be added, so a reader ignores what it does not
// know. Nothing but these lines is written to stdout.
//
// Order: start; discover; then, team by team in a fixed order, holdings and team_blocked
// as each team is planned; one plan; then, team by team, each planned session's
// session_start and its session_done, session_failed or session_skipped, with
// team_blocked when a team stops; and summary. Sessions planning leaves out (sent or
// failed in an earlier run, or held by the server) are counted on plan, not reported one
// by one, and blocks before plan are not in its totals. Each session plan
// counts ends in exactly one session_done, session_failed or session_skipped line, or is
// counted in a later team_blocked line's "skipped" (a session a team stopped on after its
// session_start included). A dry run asks no server and sends nothing: start, discover,
// plan, summary.
//
// How a run ends: summary is the last line of a run that finished, exit code 0, or 1 when
// a session failed or a team was blocked in this run. A run stopped by Ctrl-C ends with a
// summary whose "interrupted" is true, exit code 130 (143 on SIGTERM). A run that could
// not finish ends with an error line: after the summary when the upload itself had begun
// (upload_running, or signed_out for a sign-in the server retired), and as the only line
// when it could not begin (not_set_up, signed_out with no sign-in, or failed for a bad
// flag or environment). A plan line comes only when every team was planned.
//
// Fields, by type:
//   - start: dry_run, since (RFC 3339), active_window_seconds, agents: [{name,
//     display_name, history}] (history: terma can read this agent's past sessions).
//   - discover: files (transcripts found), skipped_sessions ({skip reason: count}).
//   - holdings: enrollment_id, team_id, team_label, sessions (asked about).
//   - plan: holdings_checked (false in a dry run), sessions, resume_sessions, records
//     (before content filtering in an upload), source_bytes (each session's own files on
//     disk; a session split across two teams counts under each), estimated_bytes (request
//     bytes, dry run only), skipped_sessions ({skip reason: count} of the sessions
//     planning left out: already_done, failed_previously, already_held), teams:
//     [{enrollment_id, team_id, team_label, sessions, resume_sessions, records,
//     source_bytes, estimated_bytes, skipped_sessions}]: every team with sessions to send
//     or sessions planning left out, so its sessions may be 0. A team blocked while
//     planning is counted in its team_blocked line, and is listed here too only when it
//     also left sessions out.
//   - session_start, session_done, session_failed, session_skipped: index and total
//     (the session's place in the plan), enrollment_id, team_id,
//     team_label, source_system, session_id, source_bytes; session_done adds records and
//     bytes (sent), session_failed reason (the server's words), session_skipped reason
//     (one of the skip reasons the summary counts).
//   - team_blocked: enrollment_id, team_id, team_label, reason_code (a skip reason),
//     reason (words), skipped (sessions left for another run; may be 0).
//   - summary: summary (the object -o json prints).
//   - error: error: {code, message}; the codes below.
const (
	// historyLineStart opens the run: what it asked for, and which agents have history
	// terma can read (agents[].history).
	historyLineStart = "start"
	// historyLineDiscover is discovery done: files found, and sessions left out by reason.
	historyLineDiscover = "discover"
	// historyLineHoldings is one team's server being asked which sessions it holds.
	historyLineHoldings = "holdings"
	// historyLinePlan is what is left to send, in all and per team, before any is sent.
	historyLinePlan = "plan"
	// historyLineSessionStart is a planned session about to be sent.
	historyLineSessionStart = "session_start"
	// historyLineSessionDone is a session sent whole.
	historyLineSessionDone = "session_done"
	// historyLineSessionFailed is a session the server rejected; --retry-failed sends it again.
	historyLineSessionFailed = "session_failed"
	// historyLineSessionSkipped is a session left out, "reason" one of the skip reasons
	// the summary counts.
	historyLineSessionSkipped = "session_skipped"
	// historyLineTeamBlocked is a team whose "skipped" remaining sessions wait for another run.
	historyLineTeamBlocked = "team_blocked"
	// historyLineSummary carries the summary `-o json` prints, unchanged, under "summary".
	historyLineSummary = "summary"
	// historyLineError ends a run that could not finish: {"code","message"} under "error".
	historyLineError = "error"
)

// The error codes a --json-stream run reports, each with an exit code of its own.
const (
	historyErrUploadRunning = "upload_running" // another upload holds this profile's lock
	historyErrNotSetUp      = "not_set_up"     // no `terma setup` yet, or set up for another account
	historyErrSignedOut     = "signed_out"     // not signed in, or the sign-in was retired
	historyErrFailed        = "failed"         // anything else; exit code 1
)

// historyError is a failure of `terma history upload` with its --json-stream code. Its
// message is the one text mode prints.
type historyError struct {
	code string
	exit int
	err  error
}

func (e *historyError) Error() string { return e.err.Error() }
func (e *historyError) Unwrap() error { return e.err }

// historyErrorOf is err's code and exit code.
func historyErrorOf(err error) (code string, exit int) {
	if he, ok := errors.AsType[*historyError](err); ok {
		return he.code, he.exit
	}
	return historyErrFailed, 1
}

type historyStreamAgent struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	History     bool   `json:"history"`
}

type historyStreamStart struct {
	Type                string               `json:"type"`
	DryRun              bool                 `json:"dry_run"`
	Since               string               `json:"since"`
	ActiveWindowSeconds int64                `json:"active_window_seconds"`
	Agents              []historyStreamAgent `json:"agents"`
}

type historyStreamDiscover struct {
	Type            string         `json:"type"`
	Files           int            `json:"files"`
	SkippedSessions map[string]int `json:"skipped_sessions"`
}

type historyStreamTeam struct {
	EnrollmentID string `json:"enrollment_id"`
	TeamID       string `json:"team_id"`
	TeamLabel    string `json:"team_label"`
}

type historyStreamHoldings struct {
	Type string `json:"type"`
	historyStreamTeam
	Sessions int `json:"sessions"`
}

type historyStreamPlan struct {
	Type string `json:"type"`
	history.UploadPlan
}

type historyStreamSession struct {
	Type string `json:"type"`
	// Index numbers the session within the plan, of Total.
	Index int `json:"index"`
	Total int `json:"total"`
	historyStreamTeam
	SourceSystem string `json:"source_system"`
	SessionID    string `json:"session_id"`
	SourceBytes  int64  `json:"source_bytes"` // 0 when the session's files could not be read
	Records      *int   `json:"records,omitempty"`
	Bytes        *int64 `json:"bytes,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type historyStreamTeamBlocked struct {
	Type string `json:"type"`
	historyStreamTeam
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
	Skipped    int    `json:"skipped"`
}

type historyStreamSummary struct {
	Type    string          `json:"type"`
	Summary history.Summary `json:"summary"`
}

type historyStreamFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type historyStreamError struct {
	Type  string               `json:"type"`
	Error historyStreamFailure `json:"error"`
}

// historyStream writes the --json-stream lines. A write that fails is kept and reported
// once the run ends. (A reader that closes the pipe ends the process with SIGPIPE; the
// next run resumes.)
type historyStream struct {
	enc *json.Encoder
	err error
}

func newHistoryStream(w io.Writer) *historyStream {
	return &historyStream{enc: json.NewEncoder(w)}
}

func (s *historyStream) write(v any) {
	if err := s.enc.Encode(v); err != nil && s.err == nil {
		s.err = err
	}
}

func (s *historyStream) start(dryRun bool, since time.Time, activeWindow time.Duration, reg *agents.Registry) {
	line := historyStreamStart{
		Type:                historyLineStart,
		DryRun:              dryRun,
		Since:               since.UTC().Format(time.RFC3339),
		ActiveWindowSeconds: int64(activeWindow / time.Second),
		Agents:              []historyStreamAgent{},
	}
	for _, a := range reg.Supported() {
		_, reads := a.(agents.HistorySource)
		line.Agents = append(line.Agents, historyStreamAgent{Name: a.Name(), DisplayName: a.DisplayName(), History: reads})
	}
	s.write(line)
}

func (s *historyStream) progress(ev history.ProgressEvent) {
	team := historyStreamTeam{EnrollmentID: ev.EnrollmentID, TeamID: ev.TeamID, TeamLabel: ev.RouteLabel}
	session := func(kind string) historyStreamSession {
		return historyStreamSession{
			Type:              kind,
			Index:             ev.Overall,
			Total:             ev.OverallTotal,
			historyStreamTeam: team,
			SourceSystem:      ev.SourceSystem,
			SessionID:         ev.SessionID,
			SourceBytes:       ev.SourceBytes,
		}
	}
	switch ev.Kind {
	case history.ProgressDiscover:
		skips := ev.Skips
		if skips == nil {
			skips = map[string]int{}
		}
		s.write(historyStreamDiscover{Type: historyLineDiscover, Files: ev.Total, SkippedSessions: skips})
	case history.ProgressHoldings:
		s.write(historyStreamHoldings{Type: historyLineHoldings, historyStreamTeam: team, Sessions: ev.Total})
	case history.ProgressPlan:
		if ev.Plan != nil {
			s.write(historyStreamPlan{Type: historyLinePlan, UploadPlan: *ev.Plan})
		}
	case history.ProgressSessionStart:
		s.write(session(historyLineSessionStart))
	case history.ProgressSessionDone:
		line := session(historyLineSessionDone)
		line.Records, line.Bytes = &ev.Records, &ev.Bytes
		s.write(line)
	case history.ProgressSessionFailed:
		line := session(historyLineSessionFailed)
		line.Reason = ev.Reason
		s.write(line)
	case history.ProgressSessionSkipped:
		line := session(historyLineSessionSkipped)
		line.Reason = ev.SkipReason
		s.write(line)
	case history.ProgressRouteBlocked:
		s.write(historyStreamTeamBlocked{Type: historyLineTeamBlocked, historyStreamTeam: team, ReasonCode: ev.SkipReason, Reason: ev.Reason, Skipped: ev.Skipped})
	}
}

func (s *historyStream) summary(sum history.Summary) {
	s.write(historyStreamSummary{Type: historyLineSummary, Summary: sum})
}

// fail writes err as the run's last line, its message on stderr too, and ends with err's
// exit code.
func (s *historyStream) fail(stderr io.Writer, err error) error {
	code, exit := historyErrorOf(err)
	fmt.Fprintf(stderr, "terma history upload: %s: %v\n", code, err)
	s.write(historyStreamError{Type: historyLineError, Error: historyStreamFailure{Code: code, Message: err.Error()}})
	return exitWith(exit)
}
