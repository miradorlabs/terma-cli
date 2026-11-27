package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

type streamWriteReq struct {
	data []byte
	done chan error
}

// finalWriteTimeout bounds how long a cancelled run waits to flush its final summary or
// error line when stdout is backed up by a reader that stopped draining the pipe.
const finalWriteTimeout = 250 * time.Millisecond

// historyStream writes the --json-stream lines through a dedicated writer goroutine so
// cancelling the run context interrupts a blocked stdout pipe rather than holding the
// upload lock forever. A write that fails is kept and reported once the run ends.
type historyStream struct {
	ctx        context.Context
	w          io.Writer
	reqs       chan streamWriteReq
	writerDone chan struct{}
	started    bool
	aborted    bool
	closed     bool
	err        error
}

func newHistoryStream(ctx context.Context, w io.Writer) *historyStream {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &historyStream{
		ctx:        ctx,
		w:          w,
		reqs:       make(chan streamWriteReq),
		writerDone: make(chan struct{}),
	}
	go s.runWriter()
	return s
}

func (s *historyStream) runWriter() {
	defer close(s.writerDone)
	for req := range s.reqs {
		_, err := s.w.Write(req.data)
		req.done <- err
	}
}

func (s *historyStream) cancelWrite() {
	if d, ok := s.w.(writeDeadlineSetter); ok {
		_ = d.SetWriteDeadline(time.Now())
	}
}

func (s *historyStream) abort() {
	s.aborted = true
	s.cancelWrite()
}

func (s *historyStream) close() {
	if s.closed {
		return
	}
	s.closed = true
	if !s.aborted {
		close(s.reqs)
		<-s.writerDone
		return
	}
	s.cancelWrite()
	close(s.reqs)
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.writerDone:
	case <-timer.C:
	}
}

func encodeStreamLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *historyStream) write(v any) {
	if s.aborted || s.closed || s.ctx.Err() != nil {
		return
	}
	data, err := encodeStreamLine(v)
	if err != nil {
		if s.err == nil {
			s.err = err
		}
		return
	}
	req := streamWriteReq{data: data, done: make(chan error, 1)}
	select {
	case s.reqs <- req:
	case <-s.ctx.Done():
		s.abort()
		return
	}
	select {
	case err := <-req.done:
		if err != nil && s.err == nil {
			s.err = err
		}
	case <-s.ctx.Done():
		s.abort()
	}
}

// writeFinal delivers the terminal summary or error line even after ctx is cancelled (for
// example on Ctrl-C), while bounding the wait so a stalled reader cannot block exit.
func (s *historyStream) writeFinal(v any) {
	if s.aborted || s.closed {
		return
	}
	if s.ctx.Err() == nil {
		s.write(v)
		return
	}
	data, err := encodeStreamLine(v)
	if err != nil {
		if s.err == nil {
			s.err = err
		}
		return
	}
	if d, ok := s.w.(writeDeadlineSetter); ok {
		_ = d.SetWriteDeadline(time.Now().Add(finalWriteTimeout))
	}
	req := streamWriteReq{data: data, done: make(chan error, 1)}
	timer := time.NewTimer(finalWriteTimeout)
	defer timer.Stop()
	select {
	case s.reqs <- req:
	case <-timer.C:
		s.abort()
		return
	}
	select {
	case err := <-req.done:
		if err != nil && s.err == nil {
			s.err = err
		}
	case <-timer.C:
		s.abort()
	}
}

func (s *historyStream) start(dryRun bool, since time.Time, activeWindow time.Duration, reg *agents.Registry) {
	if s.aborted || s.ctx.Err() != nil {
		return
	}
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
	if !s.aborted {
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
		s.write(historyStreamTeamBlocked{Type: historyLineTeamBlocked, historyStreamTeam: team, ReasonCode: ev.SkipReason, Reason: ev.Reason, Skipped: ev.Skipped})
	}
}

func (s *historyStream) summary(sum history.Summary) {
	s.writeFinal(historyStreamSummary{Type: historyLineSummary, Summary: sum})
}

// fail writes err as the run's last line, its message on stderr too, and ends with err's
// exit code.
func (s *historyStream) fail(stderr io.Writer, err error) error {
	code, exit := historyErrorOf(err)
	fmt.Fprintf(stderr, "terma history upload: %s: %v\n", code, err)
	s.writeFinal(historyStreamError{Type: historyLineError, Error: historyStreamFailure{Code: code, Message: err.Error()}})
	return exitWith(exit)
}

// historyUsageError reports arguments `history upload` does not take. A --json-stream run
// ends with an error line, as every other failure of one does. Flags are read up to the
// one refused, so --json-stream after it is looked for in the process's arguments.
func historyUsageError(cmd *cobra.Command, jsonStream bool, err error) error {
	if !jsonStream && !argsAskForJSONStream(cmd.Flags(), os.Args[1:]) {
		return err
	}
	s := newHistoryStream(cmd.Context(), cmd.OutOrStdout())
	defer s.close()
	return s.fail(cmd.ErrOrStderr(), err)
}

// argsAskForJSONStream reports whether args set --json-stream as a flag: not as the value of
// another of flags' flags that takes one, and not set to false.
func argsAskForJSONStream(flags *pflag.FlagSet, args []string) bool {
	// takesValue reports whether the flag a names reads the next argument as its value.
	takesValue := func(a string) bool {
		var f *pflag.Flag
		switch {
		case strings.Contains(a, "="):
			return false
		case strings.HasPrefix(a, "--"):
			f = flags.Lookup(a[2:])
		case len(a) == 2 && a[0] == '-':
			f = flags.ShorthandLookup(a[1:])
		}
		return f != nil && f.NoOptDefVal == ""
	}
	asked := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return asked
		case a == "--json-stream":
			asked = true
		case strings.HasPrefix(a, "--json-stream="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json-stream="))
			asked = err == nil && v
		case takesValue(a):
			i++
		}
	}
	return asked
}
