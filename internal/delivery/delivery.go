// Package delivery sends the hook spool to Terma: each project's events with the person's
// sign-in, naming the team, or with a server key a profile was set up with, under the
// policy in force when they leave. Once multi-org routing is active (a routing state file
// exists under StateDir), each event goes only to the enrollment routing gave it when it
// was spooled, rechecked against its profile's routing state as it leaves, and is never
// routed again (routed.go).
package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// LossNoDestination is the spool loss reason recorded when an active routed pass drops an
// event carrying no destination enrollment.
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
	// Policy is team's collection policy now (pre-activation single-team delivery only).
	Policy func(ctx context.Context, cfg *config.Config, team string) (config.Policy, error)
	// Consent asks the agent behind tool whether a reply or title it spooled may leave.
	Consent func(tool string, c hookrun.Consent) bool
	// User is who events sent to projectID at endpoint name, by Mirador user id, asked as they
	// are flushed, as the relay asks as it accepts a part; "" or a nil User names no one.
	// account is the events' own lane, the account their token was had for, "" under a server
	// key: only its user may be named, never one who signed in since.
	User func(cfg *config.Config, projectID, endpoint, account string) string
	// EnrolledUser is User under multi-org routing: who events sent to team, an enrollment
	// in profile's routing state, at endpoint under account ("" for a server key) name, by
	// Mirador user id. Nil names no one.
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
	// Routed is set when the pass ran under multi-org routing, where the fields below apply.
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

// RoutingActive reports whether multi-org routing has a state file (or an unreadable
// routing directory/file) under stateDir. Pre-activation machines have no routing state
// files and continue using single-team delivery until PR 10 cutover.
func RoutingActive(stateDir, profile string) bool {
	if stateDir == "" {
		return false
	}
	if path, err := routing.StatePath(stateDir, cmp.Or(profile, config.DefaultProfile)); err == nil {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, routing.StatesDir))
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

func (r Router) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Flush delivers everything s holds, each team with its lane's credential to its host.
// Wrong-lane events are held untouched; a failing destination backs off alone. Once a
// routing state exists under StateDir, each event goes only to its pinned enrollment
// (routed.go).
func (r Router) Flush(ctx context.Context, s *spool.Spool, cfg *config.Config, force bool, minInterval time.Duration) Result {
	var res Result
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
	}
	sender := spool.SenderFunc(p.byProject)
	if RoutingActive(r.StateDir, cfg.ProfileName) {
		res.Routed = true
		p.routes, p.ledgers, p.profiles = routeCache{}, map[string]runs.Snapshot{}, map[string]*config.Config{}
		sender = p.routed
	}
	fr := s.Flush(ctx, sender, spool.FlushOptions{Force: force, MinInterval: minInterval, Now: now})
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
	// failed and waiting are keyed as retry windows are, by project or enrollment
	// (retryDest); accepted by endpoint.
	failed, waiting, accepted map[string]bool
	// routes are the routing states kept across a routed pass's batches.
	routes routeCache
	// ledgers are the run ledgers the pass read, by session or subagent ID (pass.mixed).
	ledgers map[string]runs.Snapshot
	// profiles caches non-active profile configs loaded for cross-profile routed parcels
	// (nil entry records a missing or unreadable profile so it fails closed).
	profiles map[string]*config.Config
}

// inLane reports whether e belongs to this pass's active lane: server-key events under a
// server key, or the signed-in account's own events under a valid user-token sign-in.
func (p *pass) inLane(e spool.Event) bool {
	if p.serverKey {
		return e.NoPerson && e.Account == ""
	}
	return !e.NoPerson && routing.ValidAccount(p.account) && p.account != sendas.NoPersonLane && e.Account == p.account
}

// lane is the routing.State.Account value paired with this pass's active lane.
func (p *pass) lane() string {
	if p.serverKey {
		return sendas.NoPersonLane
	}
	return p.account
}

// byProject sends a batch on a pre-activation machine: each event to the project its
// attrs.project_id names, under that project's policy now. Any routed event (carrying
// Enrollment or Profile metadata) encountered while no routing state file exists fails
// closed instead of falling back to Profile.Team or attrs.project_id.
func (p *pass) byProject(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
	r, cfg, res := p.r, p.cfg, p.res
	byProject := map[string][]spool.Event{}
	var held []spool.Event
	policies := map[string]config.Policy{}
	policyErrors := map[string]error{}
	for _, e := range events {
		if e.Enrollment != "" || e.Profile != "" {
			if !p.inLane(e) {
				held = append(held, e)
				continue
			}
			res.Unrouted++
			held = append(held, e)
			continue
		}
		id, _ := e.Attrs[hookrun.AttrProjectID].(string)
		if id == "" {
			res.Unroutable++
			continue
		}
		// Another person's or environment's events wait for their own sign-in; a server key
		// sends only what no person collected. Judged first: no policy is asked for,
		// under this credential, for events it can't send.
		if !p.inLane(e) {
			held = append(held, e)
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
		key, err := r.credential(ctx, p.serverKey, p.account, id)
		if err != nil {
			// The keychain is locked or the auth host out of reach: retry as for a
			// refused send.
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			undelivered = append(undelivered, batch...)
			continue
		}
		if key == "" {
			// No key or sign-in yet; MaxAge already dropped anything too old to wait.
			held = append(held, batch...)
			continue
		}
		var user func(string) string
		if r.User != nil {
			user = func(endpoint string) string { return r.User(cfg, id, endpoint, p.account) }
		}
		left, err := p.deliver(ctx, id, id, key, r.Endpoint(cfg, id), user, batch)
		undelivered = append(undelivered, left...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return p.settle(held, undelivered, errs)
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

// deliver sends batch with key to endpoint for the project id, which the request names
// and failures and waits report, unless dest, whose retry window this send keeps (the
// project, or under routing its enrollment: retryDest), was refused earlier in this pass
// or waits out its retry window. It returns what stays queued, and the error a refused
// send left it with. user, when set, is who the batch names, asked only as it is sent.
func (p *pass) deliver(ctx context.Context, dest, id, key, endpoint string, user func(endpoint string) string, batch []spool.Event) ([]spool.Event, error) {
	if p.skipSend(dest, id) {
		return batch, nil
	}
	sender := &otlpSender{Endpoint: endpoint, APIKey: key, ProjectID: id, NameProject: !p.serverKey, Version: p.r.Version}
	if user != nil {
		sender.UserID = user(endpoint)
	}
	_, err := sender.Send(ctx, wire(batch))
	if recErr := p.recordOutcome(ctx, dest, id, key, endpoint, len(batch), err); recErr != nil {
		return batch, recErr
	}
	return nil, nil
}

// recordOutcome records the result of one HTTP POST to endpoint for project id under
// retry key dest. Must be called from the pass's owning goroutine.
func (p *pass) recordOutcome(ctx context.Context, dest, id, key, endpoint string, count int, err error) error {
	if err != nil {
		p.r.refused(ctx, p.serverKey, key, err)
		p.failed[dest] = true
		f := Failure{ProjectID: id, Endpoint: endpoint, Err: err}
		// A pass cut short by its own deadline learned nothing about the host.
		if ctx.Err() == nil {
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
	if e.Name == semconv.TermaAssistantMessageEvent || e.Name == semconv.TermaSessionTitleEvent {
		prompts, _ := pol.Content()
		return prompts && r.consented(e, pol.Global())
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
