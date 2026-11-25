package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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

const defaultHistorySince = "90d"

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
		sinceSpec string
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Send past local sessions that your teams do not already hold",
		Long: `Discovers past local coding agent sessions, splits each session by its
recorded working directory, routes each part through your selected teams'
policies, skips sessions each destination team already holds, and uploads the
rest tagged as historical.

Sessions modified within the last hour are skipped as still in progress.
Press Ctrl-C to stop safely; running the command again resumes unfinished
sessions first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.runHistoryUpload(cmd, sinceSpec, dryRun)
		},
	}
	cmd.Flags().StringVar(&sinceSpec, "since", defaultHistorySince, "include sessions modified within this window (e.g. 90d, 48h, or 2026-08-01)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be uploaded, to which team, and its estimated size without sending or writing state")
	return cmd
}

func (app *App) runHistoryUpload(cmd *cobra.Command, sinceSpec string, dryRun bool) error {
	now := time.Now()
	since, err := parseHistorySince(sinceSpec, now)
	if err != nil {
		return err
	}
	asJSON := false
	if app.flags.output != "" {
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
		return fmt.Errorf("%s — run `%s`", detail, routeSetupFix(serverKey))
	}

	sources := app.agents.With[agents.HistorySource]()
	capturers := app.agents.With[shape.Capturer]()
	filter := relay.NewContentFilter(capturers)
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	deliv := app.delivery()
	signIn := app.signInSource(cfg)

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
	if !asJSON && !dryRun {
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
			freshCfg, err := app.loadConfig()
			if err != nil {
				return routing.State{}, err
			}
			freshLane := app.profileLane(freshCfg)
			if freshLane != lane {
				return routing.State{}, errors.New("active sign-in account changed during upload")
			}
			fresh, found, loadErr := routing.Load(app.stateDir, freshCfg.ProfileName)
			if reason, detail := routingStateProblem(fresh, found, loadErr, lane); reason != "" {
				return routing.State{}, errors.New(detail)
			}
			return fresh, nil
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
		TeamLabel: func(team routing.Team) string { return teamLabel(names, team) },
		Since:     since,
		DryRun:    dryRun,
		Progress:  progress,
	})

	if asJSON {
		if err := output.Render(cmd.OutOrStdout(), output.FormatJSON, output.Table{}, sum); err != nil {
			return err
		}
	} else {
		renderHistorySummary(cmd.OutOrStdout(), sum)
	}

	if uploadErr != nil {
		if errors.Is(uploadErr, context.Canceled) {
			return uploadErr
		}
		if _, ok := errors.AsType[*history.SignInRetiredError](uploadErr); ok {
			return auth.ErrSignInRetired
		}
		return uploadErr
	}
	if !dryRun && hasNewHistoryFailures(sum) {
		return exitWith(1)
	}
	return nil
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

// parseHistorySince parses --since as a positive relative window (e.g. 90d, 24h) or an
// absolute date/timestamp (2006-01-02 or RFC3339).
func parseHistorySince(spec string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return time.Time{}, errors.New("--since cannot be empty")
	}
	if strings.HasSuffix(s, "d") && len(s) > 1 {
		days, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: expected a positive number of days (e.g. 90d)", spec)
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: duration must be positive", spec)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: use a duration like 90d or 48h, or a date like 2026-08-01", spec)
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
