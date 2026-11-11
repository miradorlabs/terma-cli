// Package delivery sends the hook spool to Terma: each project's events with its own key
// to its own environment's host, under the policy in force when they leave.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
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
	// The images this pass sent or let go; their bytes stay until no waiting record names them.
	var images []string
	router := spool.SenderFunc(func(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
		byProject := map[string][]spool.Event{}
		var held []spool.Event
		policies := map[string]config.Policy{}
		policyErrors := map[string]error{}
		for _, e := range events {
			id, _ := e.Attrs[hookrun.AttrProjectID].(string)
			if id == "" {
				res.Unroutable++
				images = appendImage(images, e)
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
				images = appendImage(images, e)
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
			if r.User != nil {
				sender.UserID = r.User(cfg, id, endpoint)
			}
			// An image is a request of its own, after the rest: no batch outgrows what the
			// platform takes in one, and a refused image holds up nothing sent before it. Its
			// bytes are read only for its own request.
			var requests [][]spool.Event
			if rest := slices.DeleteFunc(slices.Clone(batch), isImage); len(rest) > 0 {
				requests = append(requests, rest)
			}
			for _, e := range batch {
				if isImage(e) {
					requests = append(requests, []spool.Event{e})
				}
			}
			for i, req := range requests {
				err := r.send(ctx, sender, req)
				if err != nil {
					failed[id] = true
					f := Failure{ProjectID: id, Endpoint: endpoint, Err: err}
					// A pass cut short by its own deadline learned nothing about the host.
					if ctx.Err() == nil {
						f.RetryAt = s.DestinationFailed(id, time.Now())
					}
					res.Failures = append(res.Failures, f)
					errs = append(errs, fmt.Errorf("%s (%s): %w", id, endpoint, err))
					for _, left := range requests[i:] {
						undelivered = append(undelivered, left...)
					}
					break
				}
				s.DestinationDelivered(id, time.Now())
				accepted[endpoint] = true
				res.Sent += len(req)
				for _, e := range req {
					images = appendImage(images, e)
				}
			}
		}
		if len(undelivered) > 0 {
			return held, &spool.PartialDelivery{Undelivered: undelivered, Err: errors.Join(errs...)}
		}
		return held, nil
	})
	fr := s.Flush(ctx, router, spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
	if len(images) > 0 {
		r.forgetImages(s, images, now)
	}
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

// isImage reports whether e is a terma.prompt.image record.
func isImage(e spool.Event) bool { return e.Name == semconv.TermaPromptImageEvent }

// appendImage adds e's image id to ids, when e is an image record.
func appendImage(ids []string, e spool.Event) []string {
	if id, ok := e.Attrs[semconv.TermaImageIDKey].(string); ok && isImage(e) {
		return append(ids, id)
	}
	return ids
}

// send sends one request; an image's carries its bytes from the store. An image the platform
// refuses outright (a 400 or 413: the record, not the host or the key) goes again without
// them, saying so, rather than holding up every image after it for as long as it waits.
func (r Router) send(ctx context.Context, sender *otlpSender, req []spool.Event) error {
	if len(req) != 1 || !isImage(req[0]) {
		_, err := sender.Send(ctx, wire(req))
		return err
	}
	e := withImageData(r.StateDir, req[0])
	_, err := sender.Send(ctx, wire([]spool.Event{e}))
	var ingest *IngestError
	if _, carried := e.Attrs[semconv.TermaImageDataKey]; !carried || !errors.As(err, &ingest) ||
		ingest.Status != http.StatusBadRequest && ingest.Status != http.StatusRequestEntityTooLarge {
		return err
	}
	delete(e.Attrs, semconv.TermaImageDataKey)
	e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedRefused
	_, err = sender.Send(ctx, wire([]spool.Event{e}))
	return err
}

// forgetImages removes the stored bytes of the images ids this pass sent or let go that no
// record still in the spool names: a replayed batch, or another prompt with the same image
// still waiting, keeps them. Prune and the store's bound clear what this misses.
func (r Router) forgetImages(s *spool.Spool, ids []string, passStart time.Time) {
	waiting, err := s.Peek(math.MaxInt)
	if err != nil {
		return
	}
	named := map[string]bool{}
	for _, e := range waiting {
		if isImage(e) {
			if id, ok := e.Attrs[semconv.TermaImageIDKey].(string); ok {
				named[id] = true
			}
		}
	}
	promptimage.Sweep(r.StateDir, ids, func(id string) bool { return named[id] }, passStart)
}

// withImageData is the image record e with its bytes from the store, or saying they are gone.
func withImageData(stateDir string, e spool.Event) spool.Event {
	if _, omitted := e.Attrs[semconv.TermaImageOmittedKey]; omitted {
		return e
	}
	e.Attrs = maps.Clone(e.Attrs)
	id, _ := e.Attrs[semconv.TermaImageIDKey].(string)
	if data, ok := promptimage.Load(stateDir, id); ok {
		e.Attrs[semconv.TermaImageDataKey] = data
	} else {
		e.Attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedUnavailable
	}
	return e
}
