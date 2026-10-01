package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
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
	cmd.AddCommand(app.newSpoolFlushCommand(), newSpoolStatusCommand())
	return cmd
}

func (app *App) newSpoolFlushCommand() *cobra.Command {
	var force, quiet bool
	var minInterval time.Duration
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Deliver queued events to Terma now",
		Long: `Deliver everything queued, one request per project.

The exit status distinguishes the outcomes a script needs apart:

  0  everything queued was delivered (including "nothing was queued")
  1  delivery failed for at least one project; its events stay queued and that
     project backs off on its own, while every other project's are delivered
  2  nothing was attempted: an earlier failure's retry window is open (--force overrides)
  3  the pass ran but left work: events held for a project key or waiting out
     their project's retry window (--force overrides), or given up on for age,
     disk pressure, or being unreadable`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := app.flushSpool(cmd.Context(), force, minInterval)
			// A background flush has no reader and no caller to inform.
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
			// Held and waiting events are still queued, and every other counter is
			// a loss: either way this pass did not finish what it was asked to do.
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

// describeFlush words one flush pass: what was delivered, then a clause for every
// reason an event was not. Loss is not a delivery failure, but it is the difference
// between "coverage is complete" and "some of this session was never billed", so each
// reason is named rather than folded into one count. `terma spool flush` and doctor
// both render these, each with its own lead-in, so the two cannot disagree about what
// "held" or "pruned" means.
func describeFlush(res flushResult) (delivered string, undelivered []string) {
	delivered = fmt.Sprintf("%d event%s", res.Sent, plural(res.Sent))
	if res.Held > 0 {
		undelivered = append(undelivered, fmt.Sprintf("holding %d for a project key (run `terma install` in their repositories)", res.Held))
	}
	if res.Failed > 0 {
		undelivered = append(undelivered, fmt.Sprintf("keeping %d queued after a failed delivery", res.Failed))
	}
	for _, f := range res.Failures {
		if !f.RetryAt.IsZero() {
			undelivered = append(undelivered, fmt.Sprintf("retrying project %s after %s", f.ProjectID, f.RetryAt.Local().Format(time.Kitchen)))
		}
	}
	for _, w := range res.Waiting {
		undelivered = append(undelivered, fmt.Sprintf("retrying project %s after %s", w.ProjectID, w.RetryAt.Local().Format(time.Kitchen)))
	}
	if res.Expired > 0 {
		undelivered = append(undelivered, fmt.Sprintf("expired %d past the spool's age limit", res.Expired))
	}
	if res.Pruned > 0 {
		undelivered = append(undelivered, fmt.Sprintf("pruned %d to stay under the size limit", res.Pruned))
	}
	if res.Unroutable > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d with no project id", res.Unroutable))
	}
	if res.Dropped > 0 {
		undelivered = append(undelivered, fmt.Sprintf("dropped %d unreadable", res.Dropped))
	}
	if res.Withheld > 0 {
		undelivered = append(undelivered, fmt.Sprintf("withheld %d by capture policy", res.Withheld))
	}
	return delivered, undelivered
}

func newSpoolStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what is queued",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := openSpool()
			if s == nil {
				return errors.New("cannot open the spool directory")
			}
			n, size, err := s.Pending()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Queued events:   %d (%d KB)\n", n, size/1024)
			if next := s.NextAttempt(); !next.IsZero() && time.Now().Before(next) {
				fmt.Fprintf(out, "Backing off:     until %s (last delivery failed)\n", next.Local().Format(time.Kitchen))
			}
			windows := s.RetryWindows(time.Now())
			for _, id := range slices.Sorted(maps.Keys(windows)) {
				fmt.Fprintf(out, "Retrying:        project %s after %s — its last delivery failed (`terma spool flush --force` retries now)\n", id, windows[id].Local().Format(time.Kitchen))
			}
			keys := keystore.Projects()
			sort.Strings(keys)
			fmt.Fprintf(out, "Project keys:    %d\n", len(keys))
			unroutable, held := queuedByRouting(s, n)
			if unroutable > 0 {
				fmt.Fprintf(out, "Unroutable:      %d event%s with no project id — they cannot be delivered (they leave the queue at the next flush)\n", unroutable, plural(unroutable))
			}
			ids := make([]string, 0, len(held))
			for id := range held {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				fmt.Fprintf(out, "Held:            %d event%s for project %s — no key here yet (run `terma install` in that repository)\n", held[id], plural(held[id]), id)
			}
			return nil
		},
	}
}

// flushResult summarizes one delivery pass across projects.
//
// The counters are disjoint so a reader never has to guess which kind of loss it
// is looking at: Held events are still queued waiting for a key, Failed events are
// still queued after their project's send failed, Expired and Pruned events are
// gone for reasons of time and disk, Unroutable events can never be delivered,
// and only Dropped means the queue itself was unreadable.
type flushResult struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	Skipped                                                            bool
	Reason                                                             spool.SkipReason
	NextAttempt                                                        time.Time
	Err                                                                error
	// Failures names each project whose send failed, once per pass, in the order
	// they failed. Doctor reads it to tell its own project's failure from another's.
	Failures []projectFailure
	// Waiting names each project that was not asked this pass because an earlier
	// failure's window was still open (never under --force).
	Waiting []projectWait
	// Endpoints are the ingest hosts that accepted events, sorted.
	Endpoints []string
}

// projectFailure is one project's refused or failed delivery, and when that project
// will next be tried (zero when the pass ran out of time rather than failing).
type projectFailure struct {
	ProjectID, Endpoint string
	Err                 error
	RetryAt             time.Time
}

// projectWait is a project whose events stayed queued, unsent, until RetryAt.
type projectWait struct {
	ProjectID string
	RetryAt   time.Time
}

// Lost reports whether the pass discarded events rather than delivering or
// holding them.
func (r flushResult) Lost() bool {
	return r.Expired > 0 || r.Pruned > 0 || r.Dropped > 0 || r.Unroutable > 0
}

// flushSpool delivers everything queued. Events are routed by the project id the
// hook stamped on them, each project with its own server key from the keystore, to
// its own ingest host (projectEndpoint). Events whose project has no key here yet
// or no validated collection policy are held for a later flush; ones with no project at all can never be routed, and
// are counted apart from events that simply aged out so the two failures stay
// distinguishable. A project whose send fails keeps its events queued without
// holding up any other project's (spool.PartialDelivery), and backs off on its own:
// until its window closes it is not asked again, unless force says to.
func (app *App) flushSpool(ctx context.Context, force bool, minInterval time.Duration) (flushResult, error) {
	s := openSpool()
	if s == nil {
		return flushResult{}, errors.New("cannot open the spool directory")
	}
	cfg, err := app.loadConfig()
	if err != nil {
		return flushResult{}, err
	}
	var res flushResult
	now := time.Now()
	failed := map[string]bool{}
	waiting := map[string]bool{}
	accepted := map[string]bool{}
	router := spool.SenderFunc(func(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
		byProject := map[string][]spool.Event{}
		var held []spool.Event
		policies := map[string]config.Policy{}
		policyErrors := map[string]error{}
		for _, e := range events {
			id, _ := e.Attrs[hookrun.AttrProjectID].(string)
			if id == "" {
				res.Unroutable++
				continue
			}
			pol, checked := policies[id]
			if !checked && policyErrors[id] == nil {
				var err error
				pol, err = app.currentTeamPolicy(ctx, cfg, id)
				if err != nil {
					policyErrors[id] = err
				} else {
					policies[id] = pol
				}
			}
			if policyErrors[id] != nil {
				held = append(held, e)
				continue
			}
			if !app.spoolEventAllowed(pol, id, e) {
				res.Withheld++
				continue
			}
			byProject[id] = append(byProject[id], e)
		}
		var undelivered []spool.Event
		var errs []error
		for _, id := range slices.Sorted(maps.Keys(byProject)) {
			batch := byProject[id]
			key := keystore.Get(id)
			if key == "" {
				// No key on this machine yet. How long the wait may last is the
				// spool's business, not this router's: an event that outlives
				// MaxAge was already dropped before it got here.
				held = append(held, batch...)
				continue
			}
			if failed[id] {
				// Refused earlier in this pass: wait for the next one rather than
				// ask the same host the same question once per batch.
				undelivered = append(undelivered, batch...)
				continue
			}
			if next := s.RetryAt(id); !force && now.Before(next) {
				// Its last send failed and its window is still open. It waits
				// without holding up any other project, and without asking again.
				if !waiting[id] {
					waiting[id] = true
					res.Waiting = append(res.Waiting, projectWait{ProjectID: id, RetryAt: next})
				}
				undelivered = append(undelivered, batch...)
				continue
			}
			endpoint := app.projectEndpoint(cfg, id)
			sender := &spool.OTLPSender{Endpoint: endpoint, APIKey: key, ProjectID: id, Version: app.version}
			if _, err := sender.Send(ctx, batch); err != nil {
				failed[id] = true
				f := projectFailure{ProjectID: id, Endpoint: endpoint, Err: err}
				// A pass cut short by its own deadline learned nothing about the host.
				if ctx.Err() == nil {
					f.RetryAt = s.DestinationFailed(id, time.Now())
				}
				res.Failures = append(res.Failures, f)
				errs = append(errs, fmt.Errorf("%s (%s): %w", id, endpoint, err))
				undelivered = append(undelivered, batch...)
				continue
			}
			s.DestinationDelivered(id, time.Now())
			accepted[endpoint] = true
			res.Sent += len(batch)
		}
		if len(undelivered) > 0 {
			return held, &spool.PartialDelivery{Undelivered: undelivered, Err: errors.Join(errs...)}
		}
		return held, nil
	})
	r := s.Flush(ctx, router, spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
	res.Endpoints = slices.Sorted(maps.Keys(accepted))
	// Every kind of loss is the spool's to report, since it is the spool that
	// decides what leaves the queue. Sent is the router's own count: it is per
	// project and per send, while the spool only knows the size of the batch it
	// handed over.
	res.Held = r.Held
	res.Failed = r.Failed
	res.Expired = r.Expired
	res.Pruned = r.Pruned
	res.Dropped = r.Dropped
	res.Skipped = r.Skipped
	res.Reason = r.Reason
	res.NextAttempt = s.NextAttempt()
	res.Err = r.Err
	return res, nil
}

// Replies and titles bypass the native relay. Recheck its same policy ceiling on
// every delivery, including events captured before the organization tightened it.
func (app *App) spoolEventAllowed(org config.Policy, projectID string, e spool.Event) bool {
	org = routing.EffectivePolicy(org, projectID)
	if !org.AllowsSignal("logs") || e.Global && !org.Global() {
		return false
	}
	if org.ExcludesPath(e.Workspace, "") || org.HasExcludedPath(e.Attrs, e.Workspace) {
		return false
	}
	if e.Name == hookrun.EventAssistantMessage || e.Name == hookrun.EventSessionTitle {
		return org.IncludePrompts && len(org.ExcludePaths) == 0 && app.contentConsented(e, projectID, org.Global())
	}
	rec, recorded, err := routing.LoadRecord(projectID)
	if err != nil {
		return false
	}
	return !recorded || slices.Contains(rec.Signals, "logs")
}

// contentConsented asks the agent that spooled e whether what it said may leave: an
// event no agent owns carries nothing anyone consented to.
func (app *App) contentConsented(e spool.Event, projectID string, global bool) bool {
	tool, _ := e.Attrs[hookrun.AttrTool].(string)
	a, ok := app.agents.ForTool(tool)
	if !ok {
		return false
	}
	c, ok := a.(agents.ContentConsent)
	return ok && c.ContentConsented(projectID, global)
}

// projectEndpoint is the ingest host a project's events are delivered to: the one its
// key was stored with, because the key was minted in that project's environment and
// only that environment's ingest host accepts it. Sending every project to the active
// profile's host had a developer with one repository bound to a dev-deployment project
// and another to a production one refused with "invalid OTLP API key" on every flush,
// while their agents, which read the routing record, exported without trouble. An
// explicit --otlp-url or TERMA_OTLP_URL still wins. A key stored before its hosts were
// recorded falls back to the routing record `terma install` writes for a telemetry
// agent, and a project with neither to the active profile's host.
func (app *App) projectEndpoint(cfg *config.Config, projectID string) string {
	if app.flags.otlpURL != "" || os.Getenv("TERMA_OTLP_URL") != "" {
		return cfg.OTLPURL
	}
	if h, ok := keystore.HostsFor(projectID); ok && h.OTLP != "" {
		return h.OTLP
	}
	if rec, ok, err := routing.LoadRecord(projectID); err == nil && ok && rec.Endpoint != "" {
		return strings.TrimRight(rec.Endpoint, "/")
	}
	return cfg.OTLPURL
}

// projectAPI is the data API a project's events are read back from: its key's
// environment, like projectEndpoint. A key stored before its hosts were recorded is
// placed by its routing record — only when that names another built-in environment's
// ingest host, since a profile with a custom data API and the stock ingest host would
// otherwise be sent to the stock API. An explicit --api-url or TERMA_API_URL wins.
func (app *App) projectAPI(cfg *config.Config, projectID string) string {
	if app.flags.apiURL != "" || os.Getenv("TERMA_API_URL") != "" {
		return cfg.APIURL
	}
	if h, ok := keystore.HostsFor(projectID); ok && h.API != "" {
		return h.API
	}
	if rec, ok, err := routing.LoadRecord(projectID); err == nil && ok && strings.TrimRight(rec.Endpoint, "/") != cfg.OTLPURL {
		if e, ok := config.EndpointsByOTLP(rec.Endpoint); ok {
			return e.APIURL
		}
	}
	return cfg.APIURL
}

// queuedByRouting splits the queued events by what delivery can do with them: how
// many can never be routed (no project id), and how many are waiting per project
// that has no key on this machine. It reads the whole queue — bounded by the count
// Pending just reported, so the split adds up to what was printed above it.
func queuedByRouting(s *spool.Spool, pending int) (int, map[string]int) {
	events, err := s.Peek(pending)
	if err != nil {
		return 0, nil
	}
	unroutable := 0
	counts := map[string]int{}
	for _, e := range events {
		id, _ := e.Attrs[hookrun.AttrProjectID].(string)
		switch {
		case id == "":
			unroutable++
		case keystore.Get(id) == "":
			counts[id]++
		}
	}
	return unroutable, counts
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
