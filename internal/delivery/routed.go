package delivery

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// maxParcelConcurrency bounds concurrent HTTP POSTs across distinct enrollments in one
// routed batch so slow endpoints cannot exhaust the flush deadline before healthy sibling
// enrollments get a turn.
const maxParcelConcurrency = 4

// routed sends a batch once multi-org routing is active. Each event is gated on its lane
// first (Account / NoPerson) before touching any routing state or run ledger: another
// lane's events wait untouched until they age out. In-lane events with no destination
// enrollment are dropped and counted (no_destination / Unroutable), never sent to
// Profile.Team or re-routed. Every other event goes only to the enrollment routing pinned
// when it was spooled, rechecked against its profile's routing state (whose Account must
// match the active lane) and the session's run ledger:
//
//   - profile routing state missing, unreadable, or paired to another account: held
//     (fail closed, never falling back to Profile.Team);
//   - enrollment ended, Refused, or no longer covers the location: dropped (Discarded);
//   - session run became Mixed at or before the event's time: dropped (Mixed), unless it
//     carries no content and routes by its own checkout location (OwnLocation && runs.Locational);
//   - team expired, signing in, suspended, or binding still pending: held (Blocked);
//   - team Active and still covering it: sent per enrollment with SignIn.Token +
//     X-Mirador-Project (user token) or keys.json with no X-Mirador-Project (server key),
//     filtered by the team's content policy now, and revalidated (state generation and
//     policy expiry clock) immediately before each POST.
func (p *pass) routed(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
	now := p.r.currentTime()
	res := p.res
	lane := p.lane()
	b := &batchRoutes{
		stateDir: p.r.StateDir,
		cache:    p.routes,
		load:     p.r.loadState,
		read:     map[string]*profileRoute{},
	}
	parcels := map[parcelKey]*parcel{}
	var held, noDest []spool.Event

	for _, e := range events {
		// 1. Lane gate FIRST: wrong-lane events stay held untouched without reading
		// routing.State, run ledgers, or policies.
		if !p.inLane(e) {
			held = append(held, e)
			continue
		}
		// 2. Destination-less events (Enrollment == "" or missing Profile) are dropped
		// and counted once routing is active, never sent to Profile.Team or re-routed.
		if e.Enrollment == "" || e.Profile == "" {
			noDest = append(noDest, e)
			continue
		}
		// 3. Load routing.State, verify State.Account == lane, recheck pinned enrollment
		// + HoldKey + run-ledger Mixed state, and filter through current policy.
		pr := b.profile(e.Profile)
		team, out, action := p.admit(e, pr, lane, now)
		switch action {
		case admitHold:
			held = append(held, e)
		case admitSend:
			k := parcelKey{profile: e.Profile, enrollment: e.Enrollment}
			if parcels[k] == nil {
				parcels[k] = &parcel{profile: e.Profile, enrollment: e.Enrollment, state: pr.state, team: team}
			}
			parcels[k].raw = append(parcels[k].raw, e)
			parcels[k].events = append(parcels[k].events, out)
		}
	}

	keys := slices.Collect(maps.Keys(parcels))
	slices.SortFunc(keys, func(a, b parcelKey) int {
		return cmp.Or(strings.Compare(a.profile, b.profile), strings.Compare(a.enrollment, b.enrollment))
	})

	var jobs []parcelJob
	var undelivered []spool.Event
	var errs []error
	var userKey string
	var userKeyErr error
	userKeyFetched := false
	for _, k := range keys {
		pc := parcels[k]
		cfg, ok := p.profileConfig(pc.profile)
		if !ok || p.wrongEnvironment(cfg, pc.team) {
			held = append(held, pc.raw...)
			continue
		}
		dest := retryDest(pc.enrollment)
		if p.skipSend(dest, pc.team.TeamID) {
			undelivered = append(undelivered, pc.raw...)
			continue
		}
		var key string
		var err error
		if p.serverKey {
			key, err = p.r.credential(ctx, true, p.account, pc.team.TeamID)
		} else {
			if !userKeyFetched {
				userKey, userKeyErr = p.r.credential(ctx, false, p.account, pc.team.TeamID)
				userKeyFetched = true
			}
			key, err = userKey, userKeyErr
		}
		if err != nil {
			// The keychain is locked or the auth host out of reach: retry as for a refused send.
			errs = append(errs, fmt.Errorf("%s: %w", pc.team.TeamID, err))
			undelivered = append(undelivered, pc.raw...)
			continue
		}
		if key == "" {
			// No key or sign-in yet; MaxAge already dropped anything too old to wait.
			held = append(held, pc.raw...)
			continue
		}
		jobs = append(jobs, parcelJob{pc: pc, cfg: cfg, dest: dest, key: key})
	}

	if len(jobs) > 0 {
		outcomes := p.sendParcels(ctx, jobs, lane)
		for i, job := range jobs {
			out := outcomes[i]
			held = append(held, out.reheld...)
			if !out.sent {
				continue
			}
			if recErr := p.recordOutcome(ctx, job.dest, out.team.TeamID, job.key, out.endpoint, out.sendCount, out.err); recErr != nil {
				undelivered = append(undelivered, out.sendRaw...)
				errs = append(errs, recErr)
			}
		}
	}

	if len(noDest) > 0 {
		// Persist the loss count BEFORE spool acknowledges the batch: if losses/ is
		// unwritable, retain the events as undelivered and surface the storage error so
		// no destination-less events disappear uncounted.
		if err := p.s.CountLost(LossNoDestination, len(noDest)); err != nil {
			errs = append(errs, fmt.Errorf("record dropped events: %w", err))
			undelivered = append(undelivered, noDest...)
		} else {
			res.Unroutable += len(noDest)
		}
	}
	return p.settle(held, undelivered, errs)
}

type parcelJob struct {
	pc   *parcel
	cfg  *config.Config
	dest string
	key  string
}

type parcelOutcome struct {
	sent      bool
	team      routing.Team
	endpoint  string
	sendRaw   []spool.Event
	sendCount int
	reheld    []spool.Event
	err       error
}

// sendParcels revalidates each job immediately before POST and executes HTTP sends with
// bounded concurrency so slow enrollments cannot starve healthy sibling enrollments.
// Shared routing cache and admission counters are guarded by mu only for local checks,
// never held across network I/O; spool retry windows and result settlement run serially
// in the caller after all goroutines return.
func (p *pass) sendParcels(ctx context.Context, jobs []parcelJob, lane string) []parcelOutcome {
	outcomes := make([]parcelOutcome, len(jobs))
	limit := p.r.maxConcurrency
	if limit <= 0 {
		limit = maxParcelConcurrency
	}
	var mu sync.Mutex
	runOne := func(i int, job parcelJob) {
		mu.Lock()
		team, sendEvents, sendRaw, reheld := p.revalidate(job.pc, lane)
		var sender *otlpSender
		var wireEvents []spool.Event
		var endpoint string
		if len(sendEvents) > 0 {
			if p.wrongEnvironment(job.cfg, team) || (p.serverKey && team.TeamID != job.pc.team.TeamID) {
				reheld = append(reheld, sendRaw...)
				sendEvents = nil
			} else {
				endpoint = p.r.Endpoint(job.cfg, team.TeamID)
				sender = &otlpSender{
					Endpoint:    endpoint,
					APIKey:      job.key,
					ProjectID:   team.TeamID,
					NameProject: !p.serverKey,
					Version:     p.r.Version,
				}
				if !p.serverKey && p.r.EnrolledUser != nil {
					sender.UserID = p.r.EnrolledUser(job.pc.profile, team, endpoint, p.account)
				}
				wireEvents = wire(sendEvents)
			}
		}
		mu.Unlock()

		if len(sendEvents) == 0 {
			outcomes[i] = parcelOutcome{reheld: reheld}
			return
		}
		_, err := sender.Send(ctx, wireEvents)
		outcomes[i] = parcelOutcome{
			sent:      true,
			team:      team,
			endpoint:  endpoint,
			sendRaw:   sendRaw,
			sendCount: len(sendEvents),
			reheld:    reheld,
			err:       err,
		}
	}

	if len(jobs) == 1 || limit == 1 {
		for i, job := range jobs {
			runOne(i, job)
		}
		return outcomes
	}
	sem := make(chan struct{}, min(limit, len(jobs)))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j parcelJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			runOne(idx, j)
		}(i, job)
	}
	wg.Wait()
	return outcomes
}

type admitAction uint8

const (
	admitDrop admitAction = iota
	admitHold
	admitSend
)

// admit judges one in-lane routed event against pr and the active lane, updating p.res
// counters for drops, holds, and policy-withheld events.
func (p *pass) admit(e spool.Event, pr *profileRoute, lane string, now time.Time) (routing.Team, spool.Event, admitAction) {
	res := p.res
	if pr == nil || pr.state.Account != lane {
		res.Unrouted++
		return routing.Team{}, spool.Event{}, admitHold
	}
	team, ok := pr.state.Team(e.Enrollment)
	v, d := pr.router.Recheck(routing.Destination{EnrollmentID: e.Enrollment, Location: locationOf(e)}, now)
	switch {
	case v == routing.Discard || !ok:
		res.Discarded++
		return routing.Team{}, spool.Event{}, admitDrop
	case p.mixed(e):
		res.Mixed++
		if res.MixedBy == nil {
			res.MixedBy = map[runs.Reason]int{}
		}
		res.MixedBy[p.ledgers[e.SessionID].Why(e.Time).Reason]++
		return routing.Team{}, spool.Event{}, admitDrop
	case v == routing.Wait:
		res.Blocked++
		return routing.Team{}, spool.Event{}, admitHold
	case v != routing.SendNow || e.HoldKey != "" && !holdBound(pr.state, d, e.HoldKey, team.OrganizationID):
		res.Discarded++
		return routing.Team{}, spool.Event{}, admitDrop
	default:
		out, ok := p.r.outgoingTo(team, e)
		if !ok {
			res.Withheld++
			return routing.Team{}, spool.Event{}, admitDrop
		}
		return team, out, admitSend
	}
}

// revalidate re-checks pc against the profile's current routing state and current wall
// clock immediately before POSTing. When the state generation has not moved and the team's
// policy has not expired, it returns pc's already-filtered events directly without
// recompiling or refiltering.
func (p *pass) revalidate(pc *parcel, lane string) (routing.Team, []spool.Event, []spool.Event, []spool.Event) {
	now := p.r.currentTime()
	fresh := p.routes.load(p.r.StateDir, pc.profile, p.r.loadState)
	if fresh != nil && fresh.state.Account == lane && routing.SameGeneration(pc.state, fresh.state) && pc.team.StateAt(now) == routing.Active {
		return pc.team, pc.events, pc.raw, nil
	}
	var team routing.Team
	var sendEvents, sendRaw, reheld []spool.Event
	for _, e := range pc.raw {
		t, out, action := p.admit(e, fresh, lane, now)
		switch action {
		case admitHold:
			reheld = append(reheld, e)
		case admitSend:
			team = t
			sendEvents = append(sendEvents, out)
			sendRaw = append(sendRaw, e)
		}
	}
	return team, sendEvents, sendRaw, reheld
}

// profileConfig returns the resolved config for profile: the active pass's config when
// profile matches p.cfg.ProfileName, else the named profile loaded from ConfigDir (with
// any process-wide OTLPPinned override preserved). Missing, unreadable, or differently-
// authenticated profiles fail closed so cross-profile events never post to the wrong
// ingest endpoint.
func (p *pass) profileConfig(profile string) (*config.Config, bool) {
	if profile == p.cfg.ProfileName {
		return p.cfg, true
	}
	if cfg, cached := p.profiles[profile]; cached {
		return cfg, cfg != nil
	}
	file, err := config.LoadFile(p.r.ConfigDir)
	if err != nil || file.Profiles[profile] == nil {
		p.profiles[profile] = nil
		return nil, false
	}
	cfg, err := config.Load(p.r.ConfigDir, p.r.StateDir, config.Overrides{Profile: profile})
	if err != nil || cfg.ServerKeySignIn != p.serverKey {
		p.profiles[profile] = nil
		return nil, false
	}
	if p.r.OTLPPinned {
		cfg.OTLPURL = p.cfg.OTLPURL
	}
	p.profiles[profile] = cfg
	return cfg, true
}

// wrongEnvironment reports whether sending to team under cfg would cross environments:
// either cfg disagrees with the active pass's auth host, the enrollment belongs to a
// different auth host than cfg.AuthURL, or a server-key profile's key was set up against
// a different auth host than cfg.AuthURL.
func (p *pass) wrongEnvironment(cfg *config.Config, team routing.Team) bool {
	if cfg == nil {
		return true
	}
	if p.cfg.AuthURL != "" && cfg.AuthURL != "" && !sameHost(cfg.AuthURL, p.cfg.AuthURL) {
		return true
	}
	if cfg.AuthURL == "" {
		return false
	}
	if team.AuthURL != "" && !sameHost(team.AuthURL, cfg.AuthURL) {
		return true
	}
	if p.serverKey && !sameHost(cfg.ServerKeyAuthURL, cfg.AuthURL) {
		return true
	}
	return false
}

// retryDest is the retry window key enrollment's sends keep, isolating backoff per
// enrollment so one failing enrollment (e.g. ingest 403 ORG_NOT_ACCESSIBLE) never holds
// back sibling enrollments sharing the same user token.
func retryDest(enrollment string) string { return "enrollment/" + enrollment }

type parcelKey struct{ profile, enrollment string }

// parcel is one enrollment's events in a batch, and the state, team, and profile they go to.
type parcel struct {
	profile    string
	enrollment string
	state      routing.State
	team       routing.Team
	raw        []spool.Event
	events     []spool.Event
}

// routeCache keeps each profile's routing state and its Router across one pass's batches,
// compiling a new Router only when the state's generation or identity moves.
type routeCache map[string]*profileRoute

// profileRoute is a profile's routing state, compiled.
type profileRoute struct {
	state  routing.State
	router *routing.Router
}

// batchRoutes are the routing states one batch reads, each at most once, so every event
// of a profile in the batch is judged against the same state.
type batchRoutes struct {
	stateDir string
	cache    routeCache
	load     func(stateDir, profile string) (routing.State, bool, error)
	read     map[string]*profileRoute
}

// profile is name's routing state now, or nil while it cannot send (missing or unreadable).
func (b *batchRoutes) profile(name string) *profileRoute {
	if pr, ok := b.read[name]; ok {
		return pr
	}
	pr := b.cache.load(b.stateDir, name, b.load)
	b.read[name] = pr
	return pr
}

func (c routeCache) load(stateDir, profile string, loader func(string, string) (routing.State, bool, error)) *profileRoute {
	if loader == nil {
		loader = routing.Load
	}
	s, ok, err := loader(stateDir, profile)
	if err != nil || !ok {
		delete(c, profile)
		return nil
	}
	if have := c[profile]; have != nil && routing.SameGeneration(have.state, s) {
		return have
	}
	pr := &profileRoute{state: s, router: routing.NewRouter(s)}
	c[profile] = pr
	return pr
}

// locationOf is where e ran, as its hook named it: the checkout whose origin it carries,
// else outside git.
func locationOf(e spool.Event) routing.Location {
	if e.Repository.Origin == "" {
		return routing.Location{Kind: routing.OutsideGit}
	}
	return routing.Location{Kind: routing.Repository, Repository: e.Repository.Origin}
}

// holdBound reports whether the pending binding that held an event (holdKey) is now bound
// to organization, and d's current authority is likewise Bound to organization. An owner
// holdKey (host/owner) requires that owner binding itself; a repository holdKey
// (host/owner/repo) is satisfied by a binding on that repository, or on its owner when
// the repository has no conflicting decision of its own.
func holdBound(st routing.State, d routing.Decision, holdKey, organization string) bool {
	if d.Authority.Kind != routing.Bound || d.Authority.Organization != organization {
		return false
	}
	key := routing.Normalize(holdKey)
	if b, ok := st.Bindings[key]; ok {
		return !b.Refused && b.Organization == organization
	}
	if config.IsRepositoryKey(key) {
		b, ok := st.Bindings[routing.Owner(key)]
		return ok && !b.Refused && b.Organization == organization
	}
	return false
}

// mixed reports whether e's session's run crossed organizations at or before e's time,
// and e is not exempt: a record that carries content obeys its run's Mixed state even
// when it routes by its own location, as a reply read from an agent's turn does. Only a
// record with its own location that says where something happened and nothing else (a
// file event, a commit, a push: runs.Locational) keeps its destination. A ledger that
// cannot be read counts as Mixed from the start.
//
// Each ledger is read once a pass. A pass sends only what was spooled before it began
// (spool.Flush), so that one read is later than every event it judges, and sees every
// crossing recorded before one of them was spooled.
func (p *pass) mixed(e spool.Event) bool {
	if e.SessionID == "" || e.OwnLocation && runs.Locational(e.Name) {
		return false
	}
	l, read := p.ledgers[e.SessionID]
	if !read {
		l = p.r.ledger(e.SessionID)
		p.ledgers[e.SessionID] = l
	}
	at, mixed := l.MixedAt(e.Time)
	return mixed && (e.Time.IsZero() || !at.After(e.Time))
}

// ledger reads the run ledger of idOrAlias.
func (r Router) ledger(idOrAlias string) runs.Snapshot {
	if r.readLedger != nil {
		return r.readLedger(r.StateDir, idOrAlias)
	}
	return runs.Read(r.StateDir, idOrAlias)
}

// outgoingTo is e as it may leave for team, whose recheck found it still covers e: under
// the team's policy now, after delivery's own checks (mayLeave), without the content that
// policy withholds.
func (r Router) outgoingTo(team routing.Team, e spool.Event) (spool.Event, bool) {
	if !r.mayLeave(team.Policy, e) {
		return spool.Event{}, false
	}
	return withhold(team.Policy, e)
}

func sameHost(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }
