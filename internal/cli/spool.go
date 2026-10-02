package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/delivery"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func (app *App) newSpoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "spool",
		Short:  "Manage the local event queue hooks write to",
		Hidden: true,
		Long: `Hooks never talk to the network: they append events to a local spool and exit.
The spool is delivered later — in the background after a commit or session end,
or on demand here. A backend outage costs nothing at commit time.`,
	}
	cmd.AddCommand(app.newSpoolFlushCommand())
	return cmd
}

func (app *App) newSpoolFlushCommand() *cobra.Command {
	var force, quiet bool
	var minInterval time.Duration
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Deliver queued events to Terma now",
		Long: `Deliver everything queued, one request per team.

The exit status distinguishes the outcomes a script needs apart:

  0  everything queued was delivered (including "nothing was queued")
  1  delivery failed for at least one team; its events stay queued and that
     team backs off on its own, while every other team's are delivered
  2  nothing was attempted: an earlier failure's retry window is open (--force overrides)
  3  the pass ran but left work: events held for a team key or waiting out
     their team's retry window (--force overrides), or given up on for age,
     disk pressure, or being unreadable`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := app.flushSpool(cmd.Context(), force, minInterval)
			if quiet {
				return nil
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Skipped {
				if res.Reason == spool.SkipMinInterval {
					fmt.Fprintln(out, "Skipped: a flush ran recently (drop --min-interval to flush now).")
					return nil
				}
				fmt.Fprintf(out, "Skipped: backing off until %s after an earlier failure (use --force to retry now).\n", res.NextAttempt.Local().Format(time.Kitchen))
				return exitWith(ExitBackoff)
			}
			delivered, undelivered := describeFlush(res)
			fmt.Fprintf(out, "Flushed %s.\n", strings.Join(append([]string{delivered}, undelivered...), ", "))
			if res.Err != nil {
				return res.Err
			}
			// Held and waiting events are unfinished work, and every other counter is a loss.
			if res.Held > 0 || res.Failed > 0 || res.Lost() {
				return exitWith(ExitIncomplete)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "ignore the failure backoff")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "never print or fail (used by background flushes)")
	cmd.Flags().DurationVar(&minInterval, "min-interval", 0, "skip when a flush completed more recently than this (optional throttle for repeated invocations)")
	return cmd
}

// describeFlush words a flush pass: what was delivered, then one clause per reason an
// event was not; `terma spool flush` and doctor share it so they agree on the terms.
func describeFlush(res delivery.Result) (delivered string, undelivered []string) {
	delivered = fmt.Sprintf("%d event%s", res.Sent, plural(res.Sent))
	if res.Held > 0 {
		undelivered = append(undelivered, fmt.Sprintf("holding %d for a team key (run `terma install` in their repositories)", res.Held))
	}
	if res.Failed > 0 {
		undelivered = append(undelivered, fmt.Sprintf("keeping %d queued after a failed delivery", res.Failed))
	}
	for _, f := range res.Failures {
		if !f.RetryAt.IsZero() {
			undelivered = append(undelivered, fmt.Sprintf("retrying team %s after %s", f.ProjectID, f.RetryAt.Local().Format(time.Kitchen)))
		}
	}
	for _, w := range res.Waiting {
		undelivered = append(undelivered, fmt.Sprintf("retrying team %s after %s", w.ProjectID, w.RetryAt.Local().Format(time.Kitchen)))
	}
	if res.Expired > 0 {
		undelivered = append(undelivered, fmt.Sprintf("expired %d past the spool's age limit", res.Expired))
	}
	if res.Pruned > 0 {
		undelivered = append(undelivered, fmt.Sprintf("pruned %d to stay under the size limit", res.Pruned))
	}
	if res.Unroutable > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d with no team id", res.Unroutable))
	}
	if res.Dropped > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d unreadable", res.Dropped))
	}
	if res.Withheld > 0 {
		undelivered = append(undelivered, fmt.Sprintf("withheld %d by capture policy", res.Withheld))
	}
	return delivered, undelivered
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// delivery is how this build sends the spool.
func (app *App) delivery() delivery.Router {
	return delivery.Router{
		Version:    app.version,
		OTLPPinned: app.flags.otlpURL != "" || os.Getenv("TERMA_OTLP_URL") != "",
		APIPinned:  app.flags.apiURL != "" || os.Getenv("TERMA_API_URL") != "",
		Policy:     app.policies().Current,
		Consent: func(tool string, c hookrun.Consent) bool {
			a, ok := app.agents.ForTool(tool)
			if !ok {
				return false
			}
			consent, ok := a.(agents.ContentConsent)
			return ok && consent.ContentConsented(c)
		},
	}
}

// flushSpool delivers everything queued.
func (app *App) flushSpool(ctx context.Context, force bool, minInterval time.Duration) (delivery.Result, error) {
	s := openSpool()
	if s == nil {
		return delivery.Result{}, errors.New("cannot open the spool directory")
	}
	cfg, err := app.loadConfig()
	if err != nil {
		return delivery.Result{}, err
	}
	return app.delivery().Flush(ctx, s, cfg, force, minInterval), nil
}
