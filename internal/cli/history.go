package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/history"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const (
	defaultHistorySince        = "90d"
	defaultHistoryActiveWindow = "1h"
)

func (app *App) newHistoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Upload past coding agent sessions to Terma",
		Long: `Upload past local coding agent sessions to Terma, routed through each
selected team's collection and content policy.`,
	}
	cmd.AddCommand(app.newHistoryUploadCommand())
	return cmd
}

func (app *App) newHistoryUploadCommand() *cobra.Command {
	var (
		sinceSpec        string
		activeWindowSpec string
		dryRun           bool
		retryFailed      bool
		jsonStream       bool
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Send past local sessions that your teams do not already hold",
		Long: `Discovers past local coding agent sessions, splits each session by its
recorded working directory, routes each part through your selected teams'
policies, skips sessions each destination team already holds, and uploads the
rest tagged as historical.

Sessions modified within the last hour (--active-window) are skipped as
still in progress. Press Ctrl-C to stop safely; running the command again
resumes unfinished sessions first.

--json-stream prints one JSON object per line on stdout as the upload goes,
each with a "type": start, discover, holdings, plan (what is left to send, in
all and per team, before the first session is sent), session_start,
session_done, session_failed, session_skipped, team_blocked, and summary (the
summary -o json prints). A run that cannot finish ends with an error line,
{"code","message"} under "error"; its codes and exit codes: upload_running 4,
not_set_up 5, signed_out 6, and failed 1.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return historyUsageError(cmd, jsonStream, err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !jsonStream {
				return app.runHistoryUpload(cmd, sinceSpec, activeWindowSpec, dryRun, retryFailed, nil)
			}
			stream := newHistoryStream(cmd.OutOrStdout())
			err := app.runHistoryUpload(cmd, sinceSpec, activeWindowSpec, dryRun, retryFailed, stream)
			if _, coded := exitCodeOf(err); err != nil && !coded && !errors.Is(err, context.Canceled) {
				err = stream.fail(cmd.ErrOrStderr(), err)
			}
			if err == nil && stream.err != nil {
				return stream.err
			}
			return err
		},
	}
	cmd.Flags().StringVar(&sinceSpec, "since", defaultHistorySince, "include sessions modified within this window (e.g. 90d, 48h, or 2026-08-01)")
	cmd.Flags().StringVar(&activeWindowSpec, "active-window", defaultHistoryActiveWindow, "skip sessions modified within this window as still in progress (e.g. 1h, 30m, or 0 for none)")
	cmd.Flags().BoolVar(&jsonStream, "json-stream", false, "print progress as JSON lines on stdout, ending with the summary or an error")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return historyUsageError(cmd, jsonStream, err)
	})
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be uploaded, to which team, and its estimated size without sending or writing state")
	cmd.Flags().BoolVar(&retryFailed, "retry-failed", false, "try again the sessions a team rejected in an earlier run")
	return cmd
}

// historyUsageError reports arguments `history upload` does not take. A --json-stream run
// ends with an error line, as every other failure of one does. Flags are read up to the
// one refused, so --json-stream after it is looked for in the process's arguments.
func historyUsageError(cmd *cobra.Command, jsonStream bool, err error) error {
	if !jsonStream && !argsAskForJSONStream(os.Args[1:]) {
		return err
	}
	return newHistoryStream(cmd.OutOrStdout()).fail(cmd.ErrOrStderr(), err)
}

// argsAskForJSONStream reports whether args set --json-stream as a flag: not as the value of
// a flag that takes one, and not set to false.
func argsAskForJSONStream(args []string) bool {
	takesValue := map[string]bool{
		"--since": true, "--active-window": true, "--output": true, "-o": true, "--team": true, "-t": true,
		"--profile": true, "--env": true, "--api-url": true, "--auth-url": true, "--app-url": true, "--otlp-url": true,
	}
	asked := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return asked
		case takesValue[a]:
			i++
		case a == "--json-stream":
			asked = true
		case strings.HasPrefix(a, "--json-stream="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json-stream="))
			asked = err == nil && v
		}
	}
	return asked
}

// runHistoryUpload runs the upload, reporting to stream when it is not nil and to the
// terminal otherwise.
func (app *App) runHistoryUpload(cmd *cobra.Command, sinceSpec, activeWindowSpec string, dryRun, retryFailed bool, stream *historyStream) error {
	// git answers for the repository these name whatever folder is asked about, so every
	// session's working directory would route as that one repository.
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s is set, so every folder would read as that repository; unset it and run `terma history upload` again", name)
		}
	}
	now := time.Now()
	since, err := parseHistorySince(sinceSpec, now)
	if err != nil {
		return err
	}
	activeWindow, err := parseHistoryActiveWindow(activeWindowSpec)
	if err != nil {
		return err
	}
	asJSON := false
	if stream != nil {
		if app.flags.output != "" {
			return errors.New("--json-stream prints JSON lines; leave out --output")
		}
	} else if app.flags.output != "" {
		format, err := app.resolveFormat()
		if err != nil {
			return err
		}
		switch format {
		case output.FormatJSON:
			asJSON = true
		case output.FormatTable:
		default:
			return fmt.Errorf("history upload supports table and json output, not %s", format)
		}
	}

	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	lane := app.profileLane(cfg)
	serverKey := cfg.ServerKeySignIn || lane == sendas.NoPersonLane
	st, found, loadErr := routing.Load(app.stateDir, cfg.ProfileName)
	if stateReason, detail := routingStateProblem(st, found, loadErr, lane); stateReason != "" {
		err := fmt.Errorf("%s — run `%s`", detail, routeSetupFix(serverKey))
		switch stateReason {
		case "missing", "wrong_account", "account_unreadable":
			return &historyError{code: historyErrNotSetUp, exit: ExitNotSetUp, err: err}
		case "signed_out":
			return &historyError{code: historyErrSignedOut, exit: ExitSignedOut, err: err}
		}
		return err
	}

	sources := app.agents.With[agents.HistorySource]()
	capturers := app.agents.With[shape.Capturer]()
	filter := relay.NewContentFilter(capturers)
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	deliv := app.delivery()
	signIn := app.signInSource(cfg)
	_, laneGen := signIn.Lane()

	holdings := &history.APIHoldingsChecker{
		ClientFor: func(teamID string) (*api.Client, error) {
			if serverKey {
				key, err := keystore.Get(app.dir, teamID)
				if err != nil {
					return nil, err
				}
				if key == "" {
					return nil, errors.New("no server key stored for this team")
				}
				sc := *cfg
				sc.APIKey = key
				return api.New(&sc, api.Options{Version: app.version})
			}
			plain := *cfg
			plain.APIKey = ""
			return api.New(&plain, api.Options{Version: app.version, ProjectID: teamID})
		},
	}

	httpClient := history.NewHTTPClient()
	defer httpClient.CloseIdleConnections()
	transport := &history.Transport{
		Version: app.version,
		HTTP:    httpClient,
		SignIn:  signIn,
	}

	var progress func(history.ProgressEvent)
	if stream != nil {
		stream.start(dryRun, since, activeWindow, app.agents)
		progress = stream.progress
	} else if !asJSON && !dryRun {
		errOut := cmd.ErrOrStderr()
		progress = func(ev history.ProgressEvent) {
			renderHistoryProgress(errOut, ev)
		}
	}

	sum, uploadErr := history.Upload(cmd.Context(), history.Options{
		StateDir:         app.stateDir,
		Profile:          cfg.ProfileName,
		Account:          lane,
		ServerKey:        serverKey,
		ServerKeyTeam:    cfg.Team,
		ServerKeyAuthURL: cfg.ServerKeyAuthURL,
		AuthURL:          cfg.AuthURL,
		Version:          app.version,
		State:            st,
		ReloadState: func() (routing.State, error) {
			return app.reloadHistoryState(cfg.ProfileName, lane, laneGen)
		},
		OnOrgAccessLost: func(orgID, authURL string, epochs map[string]int64, at time.Time) {
			if serverKey || len(epochs) == 0 {
				return
			}
			committed, uerr := routing.Update(app.stateDir, cfg.ProfileName, routing.Expect{Account: lane}, func(s *routing.State) error {
				s.ApplyOrgAccessLost(orgID, authURL, epochs, at)
				return nil
			})
			if uerr == nil {
				_ = app.persistCommittedPolicies(committed)
			}
		},
		Sources:      sources,
		Filter:       filter,
		Holdings:     holdings,
		Transport:    transport,
		EndpointFor:  func(teamID string) string { return deliv.Endpoint(cfg, teamID) },
		ServerKeyFor: func(teamID string) (string, error) { return keystore.Get(app.dir, teamID) },
		UserIDFor: func(team routing.Team, endpoint string) string {
			u := app.selectedTeamUser(cfg, team, endpoint)
			if u != "" && auth.AccountKey(cfg.AuthURL, u) == lane {
				return u
			}
			return ""
		},
		TeamLabel:    func(team routing.Team) string { return teamLabel(names, team) },
		Since:        since,
		ActiveWindow: &activeWindow,
		DryRun:       dryRun,
		RetryFailed:  retryFailed,
		Progress:     progress,
	})

	switch {
	case stream != nil:
		stream.summary(sum)
	case asJSON:
		if err := output.Render(cmd.OutOrStdout(), output.FormatJSON, output.Table{}, sum); err != nil {
			return err
		}
	default:
		renderHistorySummary(cmd.OutOrStdout(), sum)
	}

	if uploadErr != nil {
		if errors.Is(uploadErr, context.Canceled) {
			return uploadErr
		}
		if _, ok := errors.AsType[*history.SignInRetiredError](uploadErr); ok {
			return &historyError{code: historyErrSignedOut, exit: ExitSignedOut, err: auth.ErrSignInRetired}
		}
		if errors.Is(uploadErr, history.ErrUploadRunning) {
			return &historyError{code: historyErrUploadRunning, exit: ExitUploadRunning, err: uploadErr}
		}
		return uploadErr
	}
	if !dryRun && hasNewHistoryFailures(sum) {
		return exitWith(1)
	}
	return nil
}

// reloadHistoryState rereads the routing state an upload rechecks against, failing when the
// active profile or its sign-in changed since the upload began: resume state is written
// under the first profile, so it must not be checked against another's routing. The
// sign-in's generation is compared as well as its lane, since a switch to someone else and
// back leaves the lane the same.
func (app *App) reloadHistoryState(profile, lane string, laneGen uint64) (routing.State, error) {
	freshCfg, err := app.loadConfig()
	if err != nil {
		return routing.State{}, err
	}
	if freshCfg.ProfileName != profile {
		return routing.State{}, errors.New("active profile changed during upload")
	}
	if app.profileLane(freshCfg) != lane {
		return routing.State{}, errors.New("active sign-in account changed during upload")
	}
	if _, gen := app.signInSource(freshCfg).Lane(); gen != laneGen {
		return routing.State{}, errors.New("active sign-in changed during upload")
	}
	fresh, found, loadErr := routing.Load(app.stateDir, freshCfg.ProfileName)
	if reason, detail := routingStateProblem(fresh, found, loadErr, lane); reason != "" {
		return routing.State{}, errors.New(detail)
	}
	return fresh, nil
}

func hasNewHistoryFailures(sum history.Summary) bool {
	if len(sum.TeamIssues) > 0 {
		return true
	}
	for _, f := range sum.Failed {
		if !f.Previously {
			return true
		}
	}
	return false
}

func renderHistoryProgress(w io.Writer, ev history.ProgressEvent) {
	switch ev.Kind {
	case history.ProgressHoldings:
		fmt.Fprintf(w, "Checking %s (%d session(s))...\n", ev.RouteLabel, ev.Total)
	case history.ProgressSessionDone:
		fmt.Fprintf(w, "  [%d/%d] uploaded %s:%s to %s (%d records, %s)\n",
			ev.Index, ev.Total, ev.SourceSystem, ev.SessionID, ev.RouteLabel, ev.Records, formatBytes(ev.Bytes))
	case history.ProgressSessionFailed:
		fmt.Fprintf(w, "  failed %s:%s for %s: %s\n", ev.SourceSystem, ev.SessionID, ev.RouteLabel, ev.Reason)
	case history.ProgressRouteBlocked:
		fmt.Fprintf(w, "  skipped %s: %s\n", ev.RouteLabel, ev.Reason)
	}
}

func renderHistorySummary(w io.Writer, sum history.Summary) {
	if sum.DryRun {
		fmt.Fprintln(w, "Dry run (nothing sent, no state written; server holdings check runs on upload):")
		if len(sum.Planned) == 0 {
			fmt.Fprintln(w, "  Nothing to upload.")
		} else {
			var totalSessions, totalRecords int
			var totalBytes int64
			for _, p := range sum.Planned {
				totalSessions += p.Sessions
				totalRecords += p.Records
				totalBytes += p.EstimatedBytes
				resumeNote := ""
				if p.ResumeSessions > 0 {
					resumeNote = fmt.Sprintf(", %d resuming", p.ResumeSessions)
				}
				fmt.Fprintf(w, "  %-32s %d session(s)%s, %d records, ~%s\n",
					p.TeamLabel, p.Sessions, resumeNote, p.Records, formatBytes(p.EstimatedBytes))
			}
			fmt.Fprintf(w, "Total: %d session(s), %d records, ~%s\n", totalSessions, totalRecords, formatBytes(totalBytes))
		}
	} else {
		if sum.Interrupted {
			fmt.Fprintln(w, "Stopped early (run `terma history upload` again to resume unfinished sessions first).")
		}
		fmt.Fprintf(w, "Sent: %d session(s), %d records, %s\n", sum.SentSessions, sum.SentRecords, formatBytes(sum.SentBytes))
	}

	if keys := history.SortedSkipKeys(sum.SkippedSessions); len(keys) > 0 {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, sum.SkippedSessions[k]))
		}
		fmt.Fprintf(w, "Skipped sessions: %s\n", strings.Join(parts, ", "))
	}
	if keys := history.SortedSkipKeys(sum.SkippedSegments); len(keys) > 0 {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, sum.SkippedSegments[k]))
		}
		fmt.Fprintf(w, "Skipped segments: %s\n", strings.Join(parts, ", "))
	}
	if sum.WithheldRecords > 0 {
		fmt.Fprintf(w, "Withheld after a Mixed cutoff: %d record(s)\n", sum.WithheldRecords)
	}
	if len(sum.Unreadable) > 0 {
		fmt.Fprintf(w, "Could not read: %s (sessions that may depend on them were skipped)\n", strings.Join(sum.Unreadable, ", "))
	}
	if len(sum.TeamIssues) > 0 {
		fmt.Fprintln(w, "Team issues:")
		for _, issue := range sum.TeamIssues {
			fmt.Fprintf(w, "  - %s (%d skipped): %s\n", issue.TeamLabel, issue.Skipped, issue.Reason)
		}
	}
	if len(sum.Failed) > 0 {
		fmt.Fprintf(w, "Failed sessions (%d):\n", len(sum.Failed))
		for _, f := range sum.Failed {
			prev := ""
			if f.Previously {
				prev = " [previous run]"
			}
			fmt.Fprintf(w, "  - %s %s:%s%s: %s\n", f.TeamLabel, f.SourceSystem, f.SessionID, prev, f.Reason)
		}
	}
	if len(sum.Failed) > 0 || sum.SkippedSessions[history.SkipFailedPreviously] > 0 {
		fmt.Fprintln(w, "Run `terma history upload --retry-failed` to try failed sessions again.")
	}
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
