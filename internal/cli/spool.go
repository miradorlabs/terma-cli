package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
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

// pushEnv is what reporting a push needs: the spool, routing state, and a flush once spooled.
func (app *App) pushEnv() hookrun.Env {
	env := hookrun.Env{StateDir: app.stateDir, ConfigDir: app.dir, Spool: app.openSpool(), Flush: spawnFlush,
		Stderr: os.Stderr, Debug: os.Getenv("TERMA_DEBUG") != "", Routing: hookrun.NewRouting("", nil)}
	// One snapshot, as a hook's profile (hookProfile): a mixed one is unreadable, and lost.
	lane, _ := readLaneAsOne(app.dir, func() string {
		cfg, err := app.loadConfig()
		if err != nil {
			return sendas.AccountUnreadable
		}
		env.Routing = hookrun.NewRouting(cfg.ProfileName, nil)
		rpath, rperr := routing.StatePath(app.stateDir, cfg.ProfileName)
		before := statOrNil(rpath)
		lane := app.profileLane(cfg)
		st, found, rerr := routing.Load(app.stateDir, cfg.ProfileName)
		stable := rperr == nil && unchanged(before, statOrNil(rpath))
		if stable && found && rerr == nil && lane != "" && lane != sendas.AccountUnreadable && st.Account == lane {
			env.Routing = hookrun.NewRouting(cfg.ProfileName, &st)
			return lane
		}
		if stable && lane == "" {
			return ""
		}
		return sendas.AccountUnreadable
	})
	if lane == sendas.AccountUnreadable {
		env.Routing = hookrun.NewRouting(env.Routing.Profile, nil)
	}
	if lane == sendas.NoPersonLane {
		env.ServerKey = true
		if env.Spool != nil {
			env.Spool.BindNoPerson()
		}
	} else {
		env.Account = lane
		if env.Spool != nil {
			env.Spool.Bind(env.Account)
		}
	}
	return env
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
  3  the pass ran but left work: events held for a sign-in, key, or until their
     team can send, or waiting out their team's retry window (--force overrides),
     or given up on for age, disk pressure, no destination, or being unreadable

Events dropped by policy are reported but are not a failure: those a team's
policy withholds or no longer collects, and those of a session that crossed
organizations.`,
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
			fix := "terma setup"
			if cfg, err := app.loadConfig(); err == nil {
				fix = app.signInFix(cfg)
			}
			delivered, undelivered := describeFlush(res, fix)
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
func describeFlush(res delivery.Result, signInFix string) (delivered string, undelivered []string) {
	delivered = fmt.Sprintf("%d event%s", res.Sent, plural(res.Sent))
	if forSignIn := res.Held - res.Blocked - res.Unrouted; forSignIn > 0 {
		undelivered = append(undelivered, fmt.Sprintf("holding %d for a sign-in that may send them (run `%s`)", forSignIn, signInFix))
	}
	if res.Blocked > 0 {
		undelivered = append(undelivered, fmt.Sprintf("holding %d until their team can send again (run `terma doctor`)", res.Blocked))
	}
	if res.Unrouted > 0 {
		undelivered = append(undelivered, fmt.Sprintf("holding %d until this machine's routing state can be read (run `terma doctor`)", res.Unrouted))
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
		if res.Routed {
			undelivered = append(undelivered, fmt.Sprintf("dropped %d with no destination", res.Unroutable))
		} else {
			undelivered = append(undelivered, fmt.Sprintf("dropped %d with no team id", res.Unroutable))
		}
	}
	if res.Dropped > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d unreadable", res.Dropped))
	}
	if res.Withheld > 0 {
		undelivered = append(undelivered, fmt.Sprintf("withheld %d by capture policy", res.Withheld))
	}
	if res.Discarded > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d their team no longer collects", res.Discarded))
	}
	if res.Mixed > 0 {
		s := fmt.Sprintf("dropped %d because their session's run is Mixed", res.Mixed)
		if len(res.MixedBy) > 0 {
			s += " (" + mixedReasonCounts(res.MixedBy) + ")"
		}
		undelivered = append(undelivered, s)
	}
	return delivered, undelivered
}

func mixedReasonCounts(by map[runs.Reason]int) string {
	reasons := slices.Collect(maps.Keys(by))
	slices.SortFunc(reasons, func(a, b runs.Reason) int { return cmp.Or(by[b]-by[a], strings.Compare(string(a), string(b))) })
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", by[r], mixedReasonWords(r)))
	}
	return strings.Join(parts, ", ")
}

func mixedReasonWords(r runs.Reason) string {
	switch r {
	case runs.Crossed:
		return "crossed organizations"
	case runs.UnknownReach:
		return "ran a shell command whose reach is not known"
	case runs.LocationBound:
		return "touched more places than the run ledger keeps"
	case runs.Aside:
		return "crossed while the run ledger was busy"
	}
	return "with a run ledger that cannot be read"
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
		ConfigDir:    app.dir,
		StateDir:     app.stateDir,
		Version:      app.version,
		OTLPPinned:   app.flags.otlpURL != "" || os.Getenv("TERMA_OTLP_URL") != "",
		EnrolledUser: app.enrolledUser,
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
	r.SignIn = app.signInSource(cfg)
	return r.Flush(ctx, s, cfg, force, minInterval), nil
}

// enrolledUser is accountUser under multi-org routing: who what terma sends to team, an
// enrollment in profile's routing state, at endpoint under account names, by Mirador user
// id. It reads user-credentials.json alone, never the keychain, and names only account's
// user in team's and profile's environment, never a server-key profile or someone who
// signed in since.
func (app *App) enrolledUser(profile string, team routing.Team, endpoint, account string) string {
	if account == "" || account == sendas.NoPersonLane {
		return ""
	}
	o := app.configOverrides()
	o.Profile = profile
	cfg, err := config.Load(app.dir, app.stateDir, o)
	if err != nil || cfg.ServerKeySignIn || !sameHost(endpoint, cfg.OTLPURL) {
		return ""
	}
	cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName)
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.CheckEnvironment(team.AuthURL) != nil {
		return ""
	}
	if auth.AccountKey(cred.AuthURL, cred.UserID) != account {
		return ""
	}
	return cred.UserID
}

var errTornDown = errors.New("terma is not set up on this machine; run `terma setup`")
