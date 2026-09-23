package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func newSpoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "spool",
		Short:  "Manage the local event queue hooks write to",
		Hidden: true,
		Long: `Hooks never talk to the network: they append events to a local spool and exit.
The spool is delivered later — in the background after a commit or session end,
or on demand here. A backend outage costs nothing at commit time.`,
	}
	cmd.AddCommand(newSpoolFlushCommand(), newSpoolStatusCommand())
	return cmd
}

func newSpoolFlushCommand() *cobra.Command {
	var force, quiet bool
	var minInterval time.Duration
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Deliver queued events to Terma now",
		Long: `Deliver everything queued, one request per project.

The exit status distinguishes the outcomes a script needs apart:

  0  everything queued was delivered (including "nothing was queued")
  1  delivery failed; the events stay queued and a backoff is recorded
  2  nothing was attempted: an earlier failure's retry window is open (--force overrides)
  3  the pass ran but left work: events held for a project key, or given up on
     for age, disk pressure, or being unreadable`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := flushSpool(cmd.Context(), force, minInterval)
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
			// Held events are still queued, and every other counter is a loss:
			// either way this pass did not finish what it was asked to do.
			if res.Held > 0 || res.Lost() {
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
// is looking at: Held events are still queued, Expired and Pruned events are
// gone for reasons of time and disk, Unroutable events can never be delivered,
// and only Dropped means the queue itself was unreadable.
type flushResult struct {
	Sent, Held, Expired, Pruned, Dropped, Unroutable int
	Skipped                                          bool
	Reason                                           spool.SkipReason
	NextAttempt                                      time.Time
	Err                                              error
}

// Lost reports whether the pass discarded events rather than delivering or
// holding them.
func (r flushResult) Lost() bool {
	return r.Expired > 0 || r.Pruned > 0 || r.Dropped > 0 || r.Unroutable > 0
}

// flushSpool delivers everything queued. Events are routed by the project id the
// hook stamped on them, each project with its own server key from the keystore.
// Events whose project has no key here yet are held for a later flush; ones with
// no project at all can never be routed, and are counted apart from events that
// simply aged out so the two failures stay distinguishable.
func flushSpool(ctx context.Context, force bool, minInterval time.Duration) (flushResult, error) {
	s := openSpool()
	if s == nil {
		return flushResult{}, errors.New("cannot open the spool directory")
	}
	cfg, err := loadConfig()
	if err != nil {
		return flushResult{}, err
	}
	var res flushResult
	now := time.Now()
	router := spool.SenderFunc(func(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
		byProject := map[string][]spool.Event{}
		var held []spool.Event
		for _, e := range events {
			id, _ := e.Attrs[hookrun.AttrProjectID].(string)
			if id == "" {
				res.Unroutable++
				continue
			}
			byProject[id] = append(byProject[id], e)
		}
		var failed []string
		for id, batch := range byProject {
			key := keystore.Get(id)
			if key == "" {
				// No key on this machine yet. How long the wait may last is the
				// spool's business, not this router's: an event that outlives
				// MaxAge was already dropped before it got here.
				held = append(held, batch...)
				continue
			}
			sender := &spool.OTLPSender{Endpoint: cfg.OTLPURL, APIKey: key, ProjectID: id, Version: Version}
			if _, err := sender.Send(ctx, batch); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", id, err))
				continue
			}
			res.Sent += len(batch)
		}
		if len(failed) > 0 {
			return nil, errors.New(strings.Join(failed, "; "))
		}
		return held, nil
	})
	r := s.Flush(ctx, router, spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
	// Every kind of loss is the spool's to report, since it is the spool that
	// decides what leaves the queue. Sent is the router's own count: it is per
	// project and per send, while the spool only knows the size of the batch it
	// handed over.
	res.Held = r.Held
	res.Expired = r.Expired
	res.Pruned = r.Pruned
	res.Dropped = r.Dropped
	res.Skipped = r.Skipped
	res.Reason = r.Reason
	res.NextAttempt = s.NextAttempt()
	res.Err = r.Err
	return res, nil
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
