package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/history"
)

// The --json-stream contract of `terma history upload`, read by Terma Desktop, is
// documented in full in docs/history-upload-stream.md.
//
// Summary of invariants enforced here:
//   - Every line on stdout is one JSON object with a "type" from the constants below.
//   - The last line of a completed stream is always exactly one of "summary" or "error".
//     If "error" is present it is last and the run failed.
//   - Fatal errors before work begins (upload_running, not_set_up, signed_out at start,
//     bad flags/env) emit only "error" (no "start", no "summary").
//   - Fatal errors after "start" emit "summary" of what happened so far, then "error".
//   - A stream ending with neither "summary" nor "error" means the process died; the
//     reader must treat that as a failure.
//   - Progress counts ("sessions", "index", "total") are session-team uploads.
//     "source_bytes" is an estimate from transcript files on disk (each transcript counted
//     once in plan.source_bytes) and is not comparable to "sent_bytes".
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
	historyErrSignedOut     = "signed_out"     // not signed in, or the sign-in was lost or retired
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
	StreamVersion       int                  `json:"stream_version"`
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
	SentBytes    *int64 `json:"sent_bytes,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type historyStreamTeamBlocked struct {
	Type string `json:"type"`
	historyStreamTeam
	Phase      string `json:"phase"`
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

type streamWriteReq struct {
	data []byte
	done chan error
}

const (
	// historyStreamVersion is incremented only for breaking schema changes to --json-stream.
	historyStreamVersion = 1
	// cancelGracePeriod is how long a cancelled run keeps writing queued lines and the
	// final summary or error line before giving up on a stalled stdout pipe.
	cancelGracePeriod = 250 * time.Millisecond
)

// historyStream writes --json-stream lines through one dedicated writer goroutine. Before
// ctx is cancelled, writes block normally; once ctx is cancelled, queued lines and the
// final line share one cancelGracePeriod measured from the cancel before giving up so a
// stalled pipe cannot keep upload.lock held.
type historyStream struct {
	ctx          context.Context
	w            io.Writer
	reqs         chan streamWriteReq
	writerDone   chan struct{}
	graceExpired chan struct{}
	stopGrace    chan struct{}
	started      bool
	finalWritten bool
	gaveUp       bool
	closed       bool
	err          error
}

func newHistoryStream(ctx context.Context, w io.Writer) *historyStream {
	return newHistoryStreamWithGrace(ctx, w, cancelGracePeriod)
}

func newHistoryStreamWithGrace(ctx context.Context, w io.Writer, grace time.Duration) *historyStream {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &historyStream{
		ctx:          ctx,
		w:            w,
		reqs:         make(chan streamWriteReq),
		writerDone:   make(chan struct{}),
		graceExpired: make(chan struct{}),
		stopGrace:    make(chan struct{}),
	}
	go s.runWriter()
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				t := time.NewTimer(grace)
				defer t.Stop()
				select {
				case <-t.C:
					close(s.graceExpired)
				case <-s.stopGrace:
				}
			case <-s.stopGrace:
			}
		}()
	}
	return s
}

func (s *historyStream) runWriter() {
	defer close(s.writerDone)
	for req := range s.reqs {
		n, err := s.w.Write(req.data)
		if err == nil && n != len(req.data) {
			err = io.ErrShortWrite
		}
		req.done <- err
	}
}

func (s *historyStream) giveUp() {
	s.gaveUp = true
	if s.err == nil {
		s.err = fmt.Errorf("stdout write timed out after cancel: %w", s.ctx.Err())
	}
}

func (s *historyStream) close() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.stopGrace)
	close(s.reqs)
	if s.gaveUp {
		return
	}
	<-s.writerDone
}

func encodeStreamLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *historyStream) write(v any) bool {
	if s.gaveUp || s.closed || s.err != nil {
		return false
	}
	select {
	case <-s.graceExpired:
		s.giveUp()
		return false
	default:
	}
	data, err := encodeStreamLine(v)
	if err != nil {
		s.err = err
		return false
	}
	req := streamWriteReq{data: data, done: make(chan error, 1)}
	select {
	case s.reqs <- req:
	case <-s.graceExpired:
		s.giveUp()
		return false
	}
	select {
	case err := <-req.done:
		if err != nil {
			s.err = err
			return false
		}
		return true
	case <-s.graceExpired:
		s.giveUp()
		return false
	}
}

func (s *historyStream) start(dryRun bool, since time.Time, activeWindow time.Duration, reg *agents.Registry) {
	line := historyStreamStart{
		Type:                historyLineStart,
		StreamVersion:       historyStreamVersion,
		DryRun:              dryRun,
		Since:               since.UTC().Format(time.RFC3339Nano),
		ActiveWindowSeconds: int64(activeWindow / time.Second),
		Agents:              []historyStreamAgent{},
	}
	for _, a := range reg.Supported() {
		_, reads := a.(agents.HistorySource)
		line.Agents = append(line.Agents, historyStreamAgent{Name: a.Name(), DisplayName: a.DisplayName(), History: reads})
	}
	if s.write(line) {
		s.started = true
	}
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
		line.Records, line.SentBytes = &ev.Records, &ev.Bytes
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
		s.write(historyStreamTeamBlocked{Type: historyLineTeamBlocked, historyStreamTeam: team, Phase: ev.Phase, ReasonCode: ev.SkipReason, Reason: ev.Reason, Skipped: ev.Skipped})
	}
}

func (s *historyStream) summary(sum history.Summary) {
	s.finalWritten = s.write(historyStreamSummary{Type: historyLineSummary, Summary: sum})
}

// fail writes err as the run's last line, its message on stderr too, and ends with err's
// exit code.
func (s *historyStream) fail(stderr io.Writer, err error) error {
	code, exit := historyErrorOf(err)
	fmt.Fprintf(stderr, "terma history upload: %s: %v\n", code, err)
	s.finalWritten = s.write(historyStreamError{Type: historyLineError, Error: historyStreamFailure{Code: code, Message: err.Error()}})
	return exitWith(exit)
}

func checkStreamSignIn(dir, profile string) error {
	ident, err := auth.LoadIdentity(dir, profile)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return &historyError{code: historyErrSignedOut, exit: ExitSignedOut, err: auth.ErrNotLoggedIn}
	}
	if err == nil && !ident.RetiredAt.IsZero() {
		return &historyError{code: historyErrSignedOut, exit: ExitSignedOut, err: auth.ErrSignInRetired}
	}
	return nil
}

func classifyHistoryNoToken(signIn *signInSource) func(context.Context, string) error {
	return func(context.Context, string) error {
		err := signIn.LastLoss()
		if errors.Is(err, auth.ErrSignInRetired) {
			return &history.SignInRetiredError{Detail: err.Error()}
		}
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return &history.SignInLostError{Err: err}
		}
		return nil
	}
}

func (app *App) runHistoryUploadStream(cmd *cobra.Command, sinceSpec, activeWindowSpec string, dryRun, retryFailed bool) error {
	stream := newHistoryStream(cmd.Context(), cmd.OutOrStdout())
	defer stream.close()
	err := app.runHistoryUpload(cmd, sinceSpec, activeWindowSpec, dryRun, retryFailed, stream)
	if _, coded := exitCodeOf(err); err != nil && !coded && !errors.Is(err, context.Canceled) {
		err = stream.fail(cmd.ErrOrStderr(), err)
	}
	stream.close()
	if err == nil {
		if stream.err != nil {
			return stream.err
		}
		if !stream.finalWritten {
			return errors.New("failed to write final stream line")
		}
	}
	return err
}
