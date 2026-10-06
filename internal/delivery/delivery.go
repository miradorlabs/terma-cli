// Package delivery sends the hook spool to Terma: each project's events with its own key
// to its own environment's host, under the policy in force when they leave.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Router delivers as one terma build.
type Router struct {
	// ConfigDir is terma's config directory, which holds the keys; StateDir is its state
	// directory, which holds the teams' policies.
	ConfigDir string
	StateDir  string
	Version   string
	// OTLPPinned is an ingest host a flag or the environment fixed for every project.
	OTLPPinned bool
	// Policy is team's collection policy now.
	Policy func(ctx context.Context, cfg *config.Config, team string) (config.Policy, error)
	// Consent asks the agent behind tool whether a reply or title it spooled may leave.
	Consent func(tool string, c hookrun.Consent) bool
}

// Result summarizes one delivery pass; its counters are disjoint, one per reason.
type Result struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	Skipped                                                            bool
	Reason                                                             spool.SkipReason
	NextAttempt                                                        time.Time
	Err                                                                error
	// Failures names each failed project once, in order; doctor tells its own from another's.
	Failures []Failure
	// Waiting names each project skipped because its retry window was open.
	Waiting   []Wait
	Endpoints []string
}

// Failure is a project's failed delivery; RetryAt is zero when the pass ran out of time.
type Failure struct {
	ProjectID, Endpoint string
	Err                 error
	RetryAt             time.Time
}

// Wait is a project skipped because its retry window was open.
type Wait struct {
	ProjectID string
	RetryAt   time.Time
}

// Lost reports whether the pass discarded events rather than delivering or holding them.
func (r Result) Lost() bool {
	return r.Expired > 0 || r.Pruned > 0 || r.Dropped > 0 || r.Unroutable > 0
}

// Flush delivers everything s holds, each project with its own key to its own host. A
// keyless or policy-less project's events are held; a failing one backs off alone.
func (r Router) Flush(ctx context.Context, s *spool.Spool, cfg *config.Config, force bool, minInterval time.Duration) Result {
	var res Result
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
				pol, err = r.Policy(ctx, cfg, id)
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
			out, ok := r.Outgoing(pol, id, e)
			if !ok {
				res.Withheld++
				continue
			}
			byProject[id] = append(byProject[id], out)
		}
		var undelivered []spool.Event
		var errs []error
		for _, id := range slices.Sorted(maps.Keys(byProject)) {
			batch := byProject[id]
			key, err := keystore.Get(r.ConfigDir, id)
			if err != nil {
				// The keychain is locked or out of reach: retry as for a refused send.
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				undelivered = append(undelivered, batch...)
				continue
			}
			if key == "" {
				// No key yet; MaxAge already dropped anything too old to wait.
				held = append(held, batch...)
				continue
			}
			if failed[id] {
				// Refused earlier in this pass: one question per host per pass.
				undelivered = append(undelivered, batch...)
				continue
			}
			if next := s.RetryAt(id); !force && now.Before(next) {
				if !waiting[id] {
					waiting[id] = true
					res.Waiting = append(res.Waiting, Wait{ProjectID: id, RetryAt: next})
				}
				undelivered = append(undelivered, batch...)
				continue
			}
			endpoint := r.Endpoint(cfg, id)
			sender := &otlpSender{Endpoint: endpoint, APIKey: key, ProjectID: id, Version: r.Version}
			if _, err := sender.Send(ctx, wire(batch)); err != nil {
				failed[id] = true
				f := Failure{ProjectID: id, Endpoint: endpoint, Err: err}
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
	fr := s.Flush(ctx, router, spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
	res.Endpoints = slices.Sorted(maps.Keys(accepted))
	// Loss is the spool's to report; Sent is the router's per-project count.
	res.Held = fr.Held
	res.Failed = fr.Failed
	res.Expired = fr.Expired
	res.Pruned = fr.Pruned
	res.Dropped = fr.Dropped
	res.Skipped = fr.Skipped
	res.Reason = fr.Reason
	res.NextAttempt = s.NextAttempt()
	res.Err = fr.Err
	return res
}

// Allowed rechecks the policy ceiling on every delivery, since hook events bypass the
// relay and may predate a tightened policy: an event from a repository the list no longer
// names, a reply or a thread's name the policy's prompts-off withholds, sends nothing. Outgoing removes the
// rest of what the policy withholds.
func (r Router) Allowed(org config.Policy, projectID string, e spool.Event) bool {
	org = routing.EffectivePolicy(r.StateDir, org, projectID)
	if org.CollectsNothing || e.Global && !org.Global() || !org.Admits(e.Repository) {
		return false
	}
	if e.Name == semconv.TermaAssistantMessageEvent || e.Name == semconv.TermaSessionTitleEvent {
		prompts, _ := org.Content()
		return prompts && r.consented(e, org.Global())
	}
	return true
}

// consented asks the agent that spooled e whether its content may leave; with no one to
// ask, it may not.
func (r Router) consented(e spool.Event, global bool) bool {
	tool, _ := e.Attrs[semconv.GenAIMainAgentNameKey].(string)
	return r.Consent != nil && r.Consent(tool, hookrun.ConsentFor(r.ConfigDir, global))
}

// Endpoint is a project's ingest host: a pinned one, else the one its key was stored with,
// since only its environment accepts the key, then the profile's.
func (r Router) Endpoint(cfg *config.Config, projectID string) string {
	if r.OTLPPinned {
		return cfg.OTLPURL
	}
	if h, ok := keystore.HostsFor(r.ConfigDir, projectID); ok && h.OTLP != "" {
		return h.OTLP
	}
	return cfg.OTLPURL
}
