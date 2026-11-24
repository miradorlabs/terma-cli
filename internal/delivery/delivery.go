// Package delivery sends the hook spool to Terma: each project's events with the person's
// sign-in, naming the team, or with a server key a profile was set up with, under the
// policy in force when they leave. Once multi-org routing is active (a routing state file
// exists under StateDir), each event goes only to the enrollment routing gave it when it
// was spooled, rechecked against its profile's routing state as it leaves, and is never
// routed again (routed.go).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// LossNoDestination is the spool loss reason recorded when a routed pass drops an event
// carrying no destination enrollment.
const LossNoDestination = "no_destination"

// Router delivers as one terma build.
type Router struct {
	// ConfigDir is terma's config directory, which holds the keys; StateDir is its state
	// directory, which holds the teams' policies and routing states.
	ConfigDir string
	StateDir  string
	Version   string
	// OTLPPinned is an ingest host a flag or the environment fixed for every project.
	OTLPPinned bool
	// Consent asks the agent behind tool whether a reply or title it spooled may leave.
	Consent func(tool string, c hookrun.Consent) bool
	// EnrolledUser is who events sent to team, an enrollment in profile's routing state,
	// at endpoint under account ("" for a server key) name, by Mirador user id. Nil names no one.
	EnrolledUser func(profile string, team routing.Team, endpoint, account string) string
	// SignIn is who events are sent as, on a profile not set up with a server key: each only
	// under the account it was collected under (spool.Event.Account), else held until it
	// ages out. Nil sends nothing that way.
	SignIn SignIn
	// readLedger reads a session's run ledger; runs.Read when nil (test hook).
	readLedger func(stateDir, idOrAlias string) runs.Snapshot
	// loadState reads a profile's routing state; routing.Load when nil (test hook).
	loadState func(stateDir, profile string) (routing.State, bool, error)
	// now returns the current wall clock; time.Now when nil (test hook).
	now func() time.Time
	// maxConcurrency bounds concurrent routed parcel POSTs; maxParcelConcurrency when <= 0.
	maxConcurrency int
}

// SignIn is the person's sign-in delivery sends with (sendas.SignIn).
type SignIn = sendas.SignIn

// Result summarizes one delivery pass; its counters are disjoint, one per reason, except
// Blocked and Unrouted, which break down why part of Held was held.
type Result struct {
	Sent, Held, Failed, Expired, Pruned, Dropped, Unroutable, Withheld int
	// Routed is true for every delivery pass after cutover.
	Routed bool
	// Blocked are held until their team can send again (expired, signing in, suspended, or
	// a pending binding); Unrouted are held because their profile's routing state is
	// missing, unreadable, or belongs to another account.
	Blocked, Unrouted int
	// Discarded were dropped by Recheck (enrollment ended, Refused, or no longer covers the
	// event's location). Mixed were dropped because their session's run turned Mixed by
	// their time, MixedBy counting them by reason ("" for an unreadable run ledger).
	Discarded, Mixed int
	MixedBy          map[runs.Reason]int
	Skipped          bool
	Reason           spool.SkipReason
	NextAttempt      time.Time
	Err              error
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
// A drop by policy is not loss: neither Withheld (what a team's policy withholds), nor
// Discarded (what its team no longer collects), nor Mixed (what a session that crossed
// organizations carried); `terma spool flush` reports each and succeeds.
func (r Result) Lost() bool {
	return r.Expired > 0 || r.Pruned > 0 || r.Dropped > 0 || r.Unroutable > 0
}

func (r Router) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Flush delivers everything s holds, each event only to the pinned enrollment routing gave
// it when it was spooled, rechecked against its profile's routing state as it leaves
// (routed.go). Missing, corrupt, or wrong-account routing state fails closed; events
// without an enrollment are dropped and counted as no_destination.
func (r Router) Flush(ctx context.Context, s *spool.Spool, cfg *config.Config, force bool, minInterval time.Duration) Result {
	var res Result
	res.Routed = true
	now := r.currentTime()
	serverKey := cfg.ServerKeySignIn
	account := ""
	if !serverKey && r.SignIn != nil {
		account = r.SignIn.Account()
	}
	p := &pass{
		r: r, s: s, cfg: cfg, force: force, now: now, res: &res,
		serverKey: serverKey, account: account,
		failed: map[string]bool{}, waiting: map[string]bool{}, accepted: map[string]bool{},
		routes: routeCache{}, ledgers: map[string]runs.Snapshot{}, profiles: map[string]*config.Config{},
	}
	fr := s.Flush(ctx, spool.SenderFunc(p.routed), spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
	res.Endpoints = slices.Sorted(maps.Keys(p.accepted))
	// Loss is the spool's to report; Sent is the router's per-destination count.
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

// pass is one Flush: what every batch it sends shares.
type pass struct {
	r         Router
	s         *spool.Spool
	cfg       *config.Config
	force     bool
	now       time.Time
	res       *Result
	serverKey bool
	account   string
	// failed and waiting are keyed as retry windows are, by enrollment (retryDest);
	// accepted by endpoint.
	failed, waiting, accepted map[string]bool
	// routes are the routing states kept across a routed pass's batches.
	routes routeCache
	// ledgers are the run ledgers the pass read, by session or subagent ID (pass.mixed).
	ledgers map[string]runs.Snapshot
	// profiles caches non-active profile configs loaded for cross-profile routed parcels
	// (nil entry records a missing or unreadable profile so it fails closed).
	profiles map[string]*config.Config
	// prunedNoDest ensures no_destination loss retention pruning runs at most once per pass.
	prunedNoDest bool
}

// skipSend reports whether dest should be deferred without sending: either it already
// failed earlier in this pass, or its per-destination retry window is still open.
func (p *pass) skipSend(dest, id string) bool {
	if p.failed[dest] {
		// Refused earlier in this pass: one question per destination per pass.
		return true
	}
	if next := p.s.RetryAt(dest); !p.force && p.now.Before(next) {
		if !p.waiting[dest] {
			p.waiting[dest] = true
			p.res.Waiting = append(p.res.Waiting, Wait{ProjectID: id, RetryAt: next})
		}
		return true
	}
	return false
}

// recordOutcome records the result of one HTTP POST to endpoint for project id under
// retry key dest. Must be called from the pass's owning goroutine.
func (p *pass) recordOutcome(ctx context.Context, dest, id, key, endpoint string, count int, err error) error {
	if err != nil {
		p.r.refused(ctx, p.serverKey, key, err)
		p.failed[dest] = true
		f := Failure{ProjectID: id, Endpoint: endpoint, Err: err}
		// A pass cut short by its own deadline learned nothing about the host; explicit
		// host errors (e.g. 403 ORG_NOT_ACCESSIBLE) still record their backoff window even
		// if a slower sibling parcel later exhausts the shared flush context.
		if ctx.Err() == nil || !isContextErr(err) {
			f.RetryAt = p.s.DestinationFailed(dest, p.r.currentTime())
		}
		p.res.Failures = append(p.res.Failures, f)
		return fmt.Errorf("%s (%s): %w", id, endpoint, err)
	}
	p.s.DestinationDelivered(dest, p.r.currentTime())
	p.accepted[endpoint] = true
	p.res.Sent += count
	return nil
}

// settle is a batch's answer to the spool: what it holds, and what it could not deliver.
func (p *pass) settle(held, undelivered []spool.Event, errs []error) ([]spool.Event, error) {
	if len(undelivered) > 0 {
		return held, &spool.PartialDelivery{Undelivered: undelivered, Err: errors.Join(errs...)}
	}
	return held, nil
}

// credential is what projectID's events are sent with: a server key a profile was set up
// with, else a fresh token of the sign-in account names. "" with no error holds them: no
// key, no sign-in, or another person's.
func (r Router) credential(ctx context.Context, serverKey bool, account, projectID string) (string, error) {
	if serverKey {
		return keystore.Get(r.ConfigDir, projectID)
	}
	if r.SignIn == nil || account == "" {
		return "", nil
	}
	return r.SignIn.Token(ctx, account)
}

// refused tells the sign-in what a failed send says of it: a token to rotate, or a
// sign-in retired. An ingest 403 ORG_NOT_ACCESSIBLE is neither: it backs off only the
// failing enrollment without calling Refused or Retired on the shared user token.
func (r Router) refused(ctx context.Context, serverKey bool, token string, err error) {
	ingest, ok := errors.AsType[*IngestError](err)
	if serverKey || r.SignIn == nil || !ok {
		return
	}
	switch {
	case ingest.Retired(): // whatever the status it comes with: never refreshed as expired
		r.SignIn.Retired(token)
	case ingest.Status == http.StatusUnauthorized:
		r.SignIn.Refused(ctx, token)
	}
}

// Allowed rechecks the policy ceiling on every delivery, since hook events bypass the
// relay and may predate a tightened policy: an event from a repository the list no longer
// names, a reply or a thread's name the policy's prompts-off withholds, sends nothing. Outgoing removes the
// rest of what the policy withholds.
func (r Router) Allowed(org config.Policy, projectID string, e spool.Event) bool {
	org = routing.EffectivePolicy(r.StateDir, org, projectID)
	return org.Admits(e.Repository) && r.mayLeave(org, e)
}

// mayLeave is delivery's own check of an event its team's policy pol covers: nothing under
// a policy collecting nothing, nothing captured machine-wide once pol no longer is, and a
// reply or a thread's name only with prompts and its agent's consent.
func (r Router) mayLeave(pol config.Policy, e spool.Event) bool {
	if pol.CollectsNothing || e.Global && !pol.Global() {
		return false
	}
	prompts, toolContent := pol.Content()
	switch e.Name {
	case semconv.TermaAssistantMessageEvent, semconv.TermaSessionTitleEvent:
		return prompts && r.consented(e, pol.Global())
	case semconv.TermaToolResultEvent:
		return toolContent && r.consented(e, pol.Global())
	}
	return true
}

// consented asks the agent that spooled e whether its content may leave; with no one to
// ask, it may not.
func (r Router) consented(e spool.Event, global bool) bool {
	tool, _ := e.Attrs[semconv.GenAIMainAgentNameKey].(string)
	return r.Consent != nil && r.Consent(tool, hookrun.ConsentFor(r.ConfigDir, global))
}

// Endpoint is a project's ingest host: a pinned one, else, under a server key, the one the
// key was stored with, since only its environment accepts the key, then the profile's. A
// person's token goes only to the profile's own host: keys.json is shared by the profiles,
// and a host another profile stored for the team is none of this sign-in's.
func (r Router) Endpoint(cfg *config.Config, projectID string) string {
	if r.OTLPPinned || !cfg.ServerKeySignIn {
		return cfg.OTLPURL
	}
	if h, ok := keystore.HostsFor(r.ConfigDir, projectID); ok && h.OTLP != "" {
		return h.OTLP
	}
	return cfg.OTLPURL
}
