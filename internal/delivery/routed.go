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
//   - team expired, signing in, or suspended: held (Blocked);
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
		// + run-ledger Mixed state, and filter through current policy.
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
		if !ok || p.wrongEnvironment(cfg, pc.team) || p.wrongServerKeyTeam(cfg, pc.team) {
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
			if out.deferred {
				undelivered = append(undelivered, out.sendRaw...)
				errs = append(errs, out.err)
				continue
			}
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
		// If the flush context already expired, spool.Flush cannot ackBatch and this batch
		// will replay; leave noDest undelivered without recording a premature loss.
		if ctx.Err() != nil {
			undelivered = append(undelivered, noDest...)
		} else if err := p.recordNoDestination(ctx, noDest); err != nil {
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
	deferred  bool
	team      routing.Team
	endpoint  string
	sendRaw   []spool.Event
	sendCount int
	reheld    []spool.Event
	err       error
}

const (
	minAckHeadroom = 50 * time.Millisecond
	maxAckHeadroom = 2 * time.Second
)

// sendParcels revalidates each job immediately before POST and executes HTTP sends with
// bounded concurrency so slow enrollments cannot starve healthy sibling enrollments.
// Shared routing cache and admission counters are guarded by mu only for local checks,
// never held across network I/O; spool retry windows and result settlement run serially
// in the caller after all goroutines return. Whenever ctx carries a deadline, sendParcels
// reserves proportional acknowledgement headroom (deferring sends if insufficient budget
// remains) and shares the send budget across waves so full-slot saturation by stalled
// hosts still lets queued healthy parcels deliver and be durably acknowledged.
func (p *pass) sendParcels(ctx context.Context, jobs []parcelJob, lane string) []parcelOutcome {
	outcomes := make([]parcelOutcome, len(jobs))
	limit := p.r.maxConcurrency
	if limit <= 0 {
		limit = maxParcelConcurrency
	}
	sendCtx := ctx
	var waveTimeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		rem := time.Until(deadline)
		headroom := min(maxAckHeadroom, max(minAckHeadroom, rem/5))
		if rem <= headroom {
			for i, job := range jobs {
				outcomes[i] = parcelOutcome{deferred: true, sendRaw: job.pc.raw, err: context.DeadlineExceeded}
			}
			return outcomes
		}
		sendBudget := rem - headroom
		var cancelSend context.CancelFunc
		sendCtx, cancelSend = context.WithDeadline(ctx, deadline.Add(-headroom))
		defer cancelSend()
		if waves := (len(jobs) + limit - 1) / limit; waves > 1 {
			waveTimeout = sendBudget / time.Duration(waves)
		}
	}

	var mu sync.Mutex
	runOne := func(i int, job parcelJob) {
		mu.Lock()
		team, sendEvents, sendRaw, reheld := p.revalidate(job.pc, lane)
		var sender *otlpSender
		var wireEvents []spool.Event
		var endpoint string
		if len(sendEvents) > 0 {
			if p.wrongEnvironment(job.cfg, team) || p.wrongServerKeyTeam(job.cfg, team) || (p.serverKey && team.TeamID != job.pc.team.TeamID) {
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
					HTTP:        p.http,
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
		reqCtx := sendCtx
		if waveTimeout > 0 {
			var cancelReq context.CancelFunc
			reqCtx, cancelReq = context.WithTimeout(sendCtx, waveTimeout)
			defer cancelReq()
		}
		_, err := sender.Send(reqCtx, wireEvents)
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
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, j parcelJob) {
			defer wg.Done()
			defer func() { <-sem }()
			runOne(idx, j)
		}(i, job)
	}
	wg.Wait()
	return outcomes
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

// wrongServerKeyTeam reports whether a server-key pass's parcel targets a team other than
// cfg's configured server-key team. Matching daemon.Teams.Resolve and heartbeat, a
// server-key profile may only send for cfg.Team even if keys.json still retains another
// team's key.
func (p *pass) wrongServerKeyTeam(cfg *config.Config, team routing.Team) bool {
	return p.serverKey && (cfg == nil || !cfg.ServerKeySignIn || cfg.Team == "" || team.TeamID != cfg.Team)
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
