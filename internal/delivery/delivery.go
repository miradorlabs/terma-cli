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
	// User is who events sent to projectID at endpoint name, by Mirador user id, asked as they
	// are flushed, as the relay asks as it accepts a part; "" or a nil User names no one.
	User func(cfg *config.Config, projectID, endpoint string) string
	// Images is the image lane's spool, which FlushImages delivers; Flush hands it any image
	// record an older build spooled with the events.
	Images *spool.Spool

	// imagePass and requestTimeout stand in for ImagePassTimeout and timeoutFor when set.
	imagePass      time.Duration
	requestTimeout func(n int) time.Duration
}

// Result summarizes one delivery pass; its counters are disjoint, one per reason.
type Result struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	// Moved counts the image records an older build spooled with the events, which the
	// events' pass handed to the image lane.
	Moved int
	// Deferred counts the images an image pass left untried for a later one: their project's
	// images wait out a window, or too little of the pass was left for one. Still queued.
	Deferred    int
	Skipped     bool
	Reason      spool.SkipReason
	NextAttempt time.Time
	Err         error
	// Failures names each failed project once, in order; doctor tells its own from another's.
	Failures []Failure
	// Stalls names each project whose image did not get through in its time, one record each:
	// news about the uplink, not the host, so the pass does not fail for it. Still queued.
	Stalls []Failure
	// Waiting names each project skipped because its retry window was open.
	Waiting   []Wait
	Endpoints []string
	// Images says the pass was the image lane's (FlushImages): its counts are images.
	Images bool
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

// errNoImageLane is a Router with no image spool to hand images to.
var errNoImageLane = errors.New("no image queue to hand images to")

// Flush delivers everything s holds, each project with its own key to its own host. A
// keyless or policy-less project's events are held; a failing one backs off alone. Images
// are not its to send: an image record an older build spooled with the events goes on to
// the image lane (Images), whose own pass sends it.
func (r Router) Flush(ctx context.Context, s *spool.Spool, cfg *config.Config, force bool, minInterval time.Duration) Result {
	var res Result
	now := time.Now()
	failed := map[string]bool{}
	waiting := map[string]bool{}
	accepted := map[string]bool{}
	router := spool.SenderFunc(func(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
		var undelivered []spool.Event
		var errs []error
		events, images := splitImages(events)
		if len(images) > 0 {
			err := errNoImageLane
			if r.Images != nil {
				err = r.Images.Append(images...)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("hand %d image record(s) to the image queue: %w", len(images), err))
				undelivered = append(undelivered, images...)
			} else {
				res.Moved += len(images)
			}
		}
		byProject, held, _ := r.route(ctx, cfg, events, &res)
		for _, id := range slices.Sorted(maps.Keys(byProject)) {
			batch := byProject[id]
			sender, err := r.senderFor(cfg, id)
			if err != nil {
				// The keychain is locked or out of reach: retry as for a refused send.
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				undelivered = append(undelivered, batch...)
				continue
			}
			if sender == nil {
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
			if _, err := sender.Send(ctx, wire(batch)); err != nil {
				failed[id] = true
				f := Failure{ProjectID: id, Endpoint: sender.Endpoint, Err: err}
				// A pass cut short by its own deadline learned nothing about the host.
				if ctx.Err() == nil {
					f.RetryAt = s.DestinationFailed(id, time.Now())
				}
				res.Failures = append(res.Failures, f)
				errs = append(errs, fmt.Errorf("%s (%s): %w", id, sender.Endpoint, err))
				undelivered = append(undelivered, batch...)
				continue
			}
			s.DestinationDelivered(id, time.Now())
			accepted[sender.Endpoint] = true
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

// route sorts events by project, each as its team's policy now lets it leave. An event
// whose policy is out of reach is held for a later pass; one with no project or that the
// policy withholds whole is counted in res and returned as gone.
func (r Router) route(ctx context.Context, cfg *config.Config, events []spool.Event, res *Result) (byProject map[string][]spool.Event, held, gone []spool.Event) {
	byProject = map[string][]spool.Event{}
	policies := map[string]config.Policy{}
	policyErrors := map[string]error{}
	for _, e := range events {
		id, _ := e.Attrs[hookrun.AttrProjectID].(string)
		if id == "" {
			res.Unroutable++
			gone = append(gone, e)
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
			gone = append(gone, e)
			continue
		}
		byProject[id] = append(byProject[id], out)
	}
	return byProject, held, gone
}

// senderFor is how project id is sent to: with its own key, to its own host. A nil sender
// is a project with no key yet; an error, a keychain locked or out of reach.
func (r Router) senderFor(cfg *config.Config, id string) (*otlpSender, error) {
	key, err := keystore.Get(r.ConfigDir, id)
	if err != nil || key == "" {
		return nil, err
	}
	endpoint := r.Endpoint(cfg, id)
	sender := &otlpSender{Endpoint: endpoint, APIKey: key, ProjectID: id, Version: r.Version, timeout: r.requestTimeout}
	if r.User != nil {
		sender.UserID = r.User(cfg, id, endpoint)
	}
	return sender, nil
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
	if e.Name == semconv.TermaAssistantMessageEvent || e.Name == semconv.TermaSessionTitleEvent || e.Name == semconv.TermaPromptImageEvent {
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
