package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/doctor"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
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
	cmd.AddCommand(app.newSpoolFlushCommand(), app.newSpoolAwaitPushCommand())
	return cmd
}

// newSpoolAwaitPushCommand is the detached half of the pre-push hook: it waits for git push
// to exit, then spools the push the hook recorded.
func (app *App) newSpoolAwaitPushCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "await-push <record>",
		Short:  "Internal: report a push once git push has exited",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hookrun.AwaitPush(cmd.Context(), app.pushEnv(), args[0])
			return nil
		},
	}
}

// pushEnv is what reporting a push needs: the spool, and a flush once it is spooled.
func (app *App) pushEnv() hookrun.Env {
	return hookrun.Env{StateDir: app.stateDir, ConfigDir: app.dir, Spool: app.openSpool(), Flush: spawnFlush,
		Stderr: os.Stderr, Debug: os.Getenv("TERMA_DEBUG") != ""}
}

func (app *App) newSpoolFlushCommand() *cobra.Command {
	var force, quiet, images bool
	var minInterval time.Duration
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Deliver queued events to Terma now",
		Long: `Deliver everything queued, one request per team.

The images a user put in a prompt wait in a queue of their own and leave in the
background, one to a request, so no event waits on an upload. --images delivers
that queue now, in a pass of up to ` + delivery.ImagePassTimeout.String() + `.

The exit status distinguishes the outcomes a script needs apart:

  0  everything queued was delivered (including "nothing was queued")
  1  delivery failed for at least one team; its events stay queued and that
     team backs off on its own, while every other team's are delivered
  2  nothing was attempted: an earlier failure's retry window is open (--force
     overrides), or another flush is already delivering the images
  3  the pass ran but left work: events held for a team key or waiting out
     their team's retry window (--force overrides), images left for a later
     pass, or events given up on for age, disk pressure, or being unreadable`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if images {
				res, err := app.flushImages(cmd.Context(), force)
				if quiet {
					return nil
				}
				if err != nil {
					return err
				}
				return reportImagePass(cmd.OutOrStdout(), res)
			}
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
			if line := describeImageQueue(app.imageQueue()); line != "" {
				fmt.Fprintln(out, line)
			}
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
	cmd.Flags().BoolVar(&images, "images", false, "deliver the queue of images users put in prompts, which otherwise leaves in the background")
	cmd.Flags().DurationVar(&minInterval, "min-interval", 0, "skip when a flush completed more recently than this (optional throttle for repeated invocations)")
	return cmd
}

// describeFlush words a flush pass: what was delivered, then one clause per reason an
// event was not; `terma spool flush` and doctor share it so they agree on the terms.
func describeFlush(res delivery.Result) (delivered string, undelivered []string) {
	delivered = fmt.Sprintf("%d event%s", res.Sent, plural(res.Sent))
	undelivered = heldClauses(res.Held, res.Failed)
	for _, f := range res.Failures {
		if !f.RetryAt.IsZero() {
			undelivered = append(undelivered, fmt.Sprintf("retrying team %s after %s", f.ProjectID, f.RetryAt.Local().Format(time.Kitchen)))
		}
	}
	for _, w := range res.Waiting {
		undelivered = append(undelivered, fmt.Sprintf("retrying team %s after %s", w.ProjectID, w.RetryAt.Local().Format(time.Kitchen)))
	}
	if res.Moved > 0 {
		undelivered = append(undelivered, fmt.Sprintf("handed %d image record%s to the image queue", res.Moved, plural(res.Moved)))
	}
	return delivered, append(undelivered, lossClauses(res.Expired, res.Pruned, res.Unroutable, res.Dropped, res.Withheld)...)
}

// describeImagePass words an image pass as describeFlush words the events', telling an
// image that did not finish uploading (the uplink) from a failed delivery (the host).
func describeImagePass(res delivery.ImageResult) (delivered string, undelivered []string) {
	delivered = fmt.Sprintf("%d image%s", res.Sent, plural(res.Sent))
	undelivered = heldClauses(res.Held, res.Failed)
	for _, f := range res.Failures {
		if !f.RetryAt.IsZero() {
			undelivered = append(undelivered, fmt.Sprintf("retrying team %s's images after %s", f.ProjectID, f.RetryAt.Local().Format(time.Kitchen)))
		}
	}
	for _, st := range res.Stalls {
		undelivered = append(undelivered, fmt.Sprintf("an image for team %s did not finish uploading in its time; its images retry after %s",
			st.ProjectID, st.RetryAt.Local().Format(time.Kitchen)))
	}
	for _, w := range res.Waiting {
		undelivered = append(undelivered, fmt.Sprintf("retrying team %s's images after %s", w.ProjectID, w.RetryAt.Local().Format(time.Kitchen)))
	}
	if res.Deferred > 0 {
		undelivered = append(undelivered, fmt.Sprintf("leaving %d for a later pass", res.Deferred))
	}
	return delivered, append(undelivered, lossClauses(res.Expired, res.Pruned, res.Unroutable, res.Dropped, res.Withheld)...)
}

// reportImagePass prints an image pass and returns its exit status, as the command does
// for the events'.
func reportImagePass(out io.Writer, res delivery.ImageResult) error {
	if res.Skipped {
		fmt.Fprintln(out, "Skipped: another flush is already delivering the images.")
		return exitWith(ExitBackoff)
	}
	delivered, undelivered := describeImagePass(res)
	fmt.Fprintf(out, "Flushed %s.\n", strings.Join(append([]string{delivered}, undelivered...), ", "))
	if res.Err != nil {
		return res.Err
	}
	if res.Held > 0 || res.Failed > 0 || res.Deferred > 0 || len(res.Stalls) > 0 ||
		res.Expired > 0 || res.Pruned > 0 || res.Dropped > 0 || res.Unroutable > 0 {
		return exitWith(ExitIncomplete)
	}
	return nil
}

// heldClauses word what a pass kept queued: held for a key, or after a failure.
func heldClauses(held, failed int) []string {
	var clauses []string
	if held > 0 {
		clauses = append(clauses, fmt.Sprintf("holding %d for a team key (run `terma setup`)", held))
	}
	if failed > 0 {
		clauses = append(clauses, fmt.Sprintf("keeping %d queued after a failed delivery", failed))
	}
	return clauses
}

// lossClauses word what a pass let go without sending it.
func lossClauses(expired, pruned, unroutable, dropped, withheld int) []string {
	var clauses []string
	if expired > 0 {
		clauses = append(clauses, fmt.Sprintf("expired %d past the spool's age limit", expired))
	}
	if pruned > 0 {
		clauses = append(clauses, fmt.Sprintf("pruned %d to stay under the size limit", pruned))
	}
	if unroutable > 0 {
		clauses = append(clauses, fmt.Sprintf("dropped %d with no team id", unroutable))
	}
	if dropped > 0 {
		clauses = append(clauses, fmt.Sprintf("dropped %d unreadable", dropped))
	}
	if withheld > 0 {
		clauses = append(clauses, fmt.Sprintf("withheld %d by capture policy", withheld))
	}
	return clauses
}

// describeImageQueue words what the image queue holds and each team's images window, for
// the events' flush, which leaves the images to a pass of their own; "" when it holds none.
func describeImageQueue(q doctor.ImageQueue) string {
	if q.Queued == 0 {
		return ""
	}
	line := fmt.Sprintf("Images: %d queued, delivered in the background (rerun with --images to deliver them now)", q.Queued)
	for _, id := range slices.Sorted(maps.Keys(q.Windows)) {
		why := "a failed delivery"
		if q.Stalled[id] {
			why = "an upload that did not finish in its time"
		}
		line += fmt.Sprintf("; team %s's wait until %s, after %s", id, q.Windows[id].Local().Format(time.Kitchen), why)
	}
	return line + "."
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
		ConfigDir:  app.dir,
		StateDir:   app.stateDir,
		Version:    app.version,
		OTLPPinned: app.flags.otlpURL != "" || os.Getenv("TERMA_OTLP_URL") != "",
		Policy:     app.policies().Current,
		User:       app.teamUser,
		Images:     app.openImageSpool(),
		SetUp:      func() bool { return claim.Enabled(app.stateDir) },
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
	// Teardown retires the relay's token; a flush started across it sends nothing.
	if !claim.Enabled(app.stateDir) {
		return delivery.Result{}, errTornDown
	}
	s := app.openSpool()
	if s == nil {
		return delivery.Result{}, errors.New("cannot open the spool directory")
	}
	// A push whose detached reporter never finished is reported here, and sent now.
	env := app.pushEnv()
	env.Flush = nil
	hookrun.SweepPushes(ctx, env)
	cfg, err := app.loadConfig()
	if err != nil {
		return delivery.Result{}, err
	}
	r := app.delivery()
	// A hook that found the policy stale started this flush to refresh it (hookrun refreshPolicy).
	if cfg.Policy.Stale(time.Now()) {
		r.Policy = refreshOnce(ctx, cfg, cfg.Policy.Team(), r.Policy)
	}
	res := r.Flush(ctx, s, cfg, force, minInterval)
	// Images leave in a pass of their own, detached, so no event waits on an upload.
	if r.Images != nil && app.startImageFlush != nil {
		if n, _, err := r.Images.Pending(); err == nil && n > 0 {
			app.startImageFlush()
		}
	}
	return res, nil
}

// flushImages delivers the image queue, in a pass of its own (delivery.ImagePassTimeout).
func (app *App) flushImages(ctx context.Context, force bool) (delivery.ImageResult, error) {
	if !claim.Enabled(app.stateDir) {
		return delivery.ImageResult{}, errTornDown
	}
	r := app.delivery()
	if r.Images == nil {
		return delivery.ImageResult{}, errors.New("cannot open the image queue")
	}
	cfg, err := app.loadConfig()
	if err != nil {
		return delivery.ImageResult{}, err
	}
	return r.FlushImages(ctx, app.openSpool(), cfg, force), nil
}

// imageQueue is the image lane's spool as the events' flush and doctor report it.
func (app *App) imageQueue() doctor.ImageQueue {
	s := app.openImageSpool()
	if s == nil {
		return doctor.ImageQueue{}
	}
	n, _, _ := s.Pending()
	now := time.Now()
	stalled := map[string]bool{}
	for id := range s.StalledWindows(now) {
		stalled[id] = true
	}
	return doctor.ImageQueue{Queued: n, Windows: s.RetryWindows(now), Stalled: stalled}
}

var errTornDown = errors.New("terma is not set up on this machine; run `terma setup`")

type policyFunc = func(context.Context, *config.Config, string) (config.Policy, error)

// refreshOnce refreshes team's policy now, even with nothing queued for it (a hook outside
// the list queues nothing), and answers delivery's lookups for team with that result.
func refreshOnce(ctx context.Context, cfg *config.Config, team string, current policyFunc) policyFunc {
	pol, err := current(ctx, cfg, team)
	return func(ctx context.Context, cfg *config.Config, t string) (config.Policy, error) {
		if t == team {
			return pol, err
		}
		return current(ctx, cfg, t)
	}
}
