package delivery

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// routed sends a batch on a migrated machine. Events are grouped by the profile whose
// routing state routed them, and each goes only to the enrollment it was spooled with or,
// queued before the migration, to the cutover enrollment its project maps to
// (routing.Migration.Legacy). It is never routed again. Its recheck against that state now
// (routing.Router.Recheck, then the run ledger) decides the rest, the first match winning:
//
//   - its enrollment ended, is Refused, or no longer covers it: dropped;
//   - its run became Mixed at or before its time: dropped, unless it says only where
//     something happened and routes by a location of its own (runs.Locational);
//   - its team expired, signing in or suspended, or its binding still pending: held;
//   - its team Active and still covering it: sent with that enrollment's key, filtered by
//     the team's content rules now, or held while that key is missing or locked away.
//
// A profile whose routing state is missing, unreadable or not yet committed sends nothing:
// its events stay queued.
func (p *pass) routed(ctx context.Context, events []spool.Event) ([]spool.Event, error) {
	now := time.Now()
	res := p.res
	b := &batchRoutes{stateDir: p.r.StateDir, configDir: p.r.ConfigDir, active: p.cfg.ProfileName, cache: p.routes, read: map[string]*profileRoute{}}
	parcels := map[string]*parcel{}
	var held []spool.Event
	for _, queued := range events {
		pr, e, found := b.destination(queued)
		switch found {
		case noTeam:
			res.Unroutable++
			continue
		case nowhere:
			res.Discarded++
			continue
		case unreadable:
			res.Unrouted++
			held = append(held, e)
			continue
		}
		team := pr.state.Team(e.Enrollment)
		v, d := pr.router.Recheck(routing.Destination{EnrollmentID: e.Enrollment, Location: locationOf(e)}, now)
		switch {
		case v == routing.Discard || team == nil:
			res.Discarded++
		case p.mixed(e):
			res.Mixed++
		case v == routing.Wait:
			res.Blocked++
			held = append(held, e)
		case v != routing.SendNow || e.HoldKey != "" && !boundTo(d, team.OrganizationID):
			// Held for a pending binding: sent only once it is bound to the holding team's
			// organization, which a send to that team already implies.
			res.Discarded++
		default:
			out, ok := p.r.outgoingTo(*team, e)
			if !ok {
				res.Withheld++
				continue
			}
			if parcels[e.Enrollment] == nil {
				parcels[e.Enrollment] = &parcel{profile: e.Profile, team: *team}
			}
			parcels[e.Enrollment].events = append(parcels[e.Enrollment].events, out)
		}
	}
	var undelivered []spool.Event
	var errs []error
	for _, enrollment := range slices.Sorted(maps.Keys(parcels)) {
		pc := parcels[enrollment]
		// Only the enrollment's own key, never another team's or the legacy index's.
		key, hosts, err := keystore.GetTeam(p.r.ConfigDir, enrollment)
		if err != nil {
			// The keychain is locked or out of reach: retry as for a refused send.
			errs = append(errs, fmt.Errorf("%s: %w", pc.team.TeamID, err))
			undelivered = append(undelivered, pc.events...)
			continue
		}
		if key == "" {
			// The relay creates it with the organization's sign-in; until then, held.
			held = append(held, pc.events...)
			continue
		}
		endpoint := p.cfg.OTLPURL
		if !p.r.OTLPPinned && hosts.OTLP != "" {
			endpoint = hosts.OTLP
		}
		var user func(string) string
		if p.r.EnrolledUser != nil {
			user = func(endpoint string) string { return p.r.EnrolledUser(pc.profile, pc.team, hosts, endpoint) }
		}
		left, err := p.deliver(ctx, pc.team.TeamID, key, endpoint, user, pc.events)
		undelivered = append(undelivered, left...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return p.settle(held, undelivered, errs)
}

// parcel is one enrollment's events in a batch, and the team and profile they go to.
type parcel struct {
	profile string
	team    routing.Team
	events  []spool.Event
}

// lookup is what finding an event's routing state came to.
type lookup int

const (
	// found: the state is read and the event names its enrollment.
	found lookup = iota
	// unreadable: a state that routes it, or may, cannot be read now; it waits.
	unreadable
	// nowhere: no state routes it anywhere; it is dropped.
	nowhere
	// noTeam: it names no team at all, as an event never could; it is dropped, as before.
	noTeam
)

// destination is the routing state that routed e, and e with the destination it is held to.
// An event queued before the migration names only its project (attrs.project_id): it takes
// the cutover enrollment that project maps to, its first and only routing, and keeps it
// from then on.
func (b *batchRoutes) destination(e spool.Event) (*profileRoute, spool.Event, lookup) {
	project, _ := e.Attrs[hookrun.AttrProjectID].(string)
	switch {
	case e.Enrollment != "":
	case project != "":
		name, enrollment, held := b.legacy(e.Profile, project)
		switch {
		case held:
			return nil, e, unreadable
		case enrollment == "":
			return nil, e, nowhere
		}
		e.Profile, e.Enrollment = name, enrollment
	case e.Profile == "":
		return nil, e, noTeam
	default:
		// Routed, but to no team: nothing could send it.
		return nil, e, nowhere
	}
	if e.Profile == "" {
		return nil, e, nowhere
	}
	pr := b.profile(e.Profile)
	if pr == nil {
		return nil, e, unreadable
	}
	return pr, e, found
}

// legacy is the profile whose routing state maps project, queued before the migration, and
// the cutover enrollment it maps to: profile's own when the event names one, else the
// active profile's first, then the others' by name. held is set when a state that might
// map it cannot be read now.
func (b *batchRoutes) legacy(profile, project string) (name, enrollment string, held bool) {
	names := []string{profile}
	if profile == "" {
		var err error
		if names, err = b.profiles(); err != nil {
			return "", "", true
		}
	}
	for _, name := range names {
		pr := b.profile(name)
		if pr == nil {
			held = true
			continue
		}
		if m := pr.state.Migration; m != nil && m.Legacy[project] != "" {
			return name, m.Legacy[project], false
		}
	}
	return "", "", held
}

// profiles are config.json's profiles, the active one first.
func (b *batchRoutes) profiles() ([]string, error) {
	f, err := config.LoadFile(b.configDir)
	if err != nil {
		return nil, err
	}
	others := slices.DeleteFunc(slices.Sorted(maps.Keys(f.Profiles)), func(n string) bool { return n == b.active })
	if b.active == "" {
		return others, nil
	}
	return append([]string{b.active}, others...), nil
}

// routeCache keeps each profile's routing state and its Router across one pass's batches,
// compiling a new Router only when the state's generation moves.
type routeCache map[string]*profileRoute

// profileRoute is a profile's routing state, compiled.
type profileRoute struct {
	state  routing.State
	router *routing.Router
}

// batchRoutes are the routing states one batch reads, each at most once, so every event
// of a profile in the batch is judged against the same state.
type batchRoutes struct {
	stateDir, configDir string
	// active is the profile whose legacy projects are looked up first.
	active string
	cache  routeCache
	read   map[string]*profileRoute
}

// profile is name's routing state now, or nil while it sends nothing: missing, unreadable,
// or still being prepared by the migration.
func (b *batchRoutes) profile(name string) *profileRoute {
	if pr, ok := b.read[name]; ok {
		return pr
	}
	pr := b.cache.load(b.stateDir, name)
	b.read[name] = pr
	return pr
}

func (c routeCache) load(stateDir, profile string) *profileRoute {
	s, ok, err := routing.LoadReady(stateDir, profile)
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

// boundTo reports whether d's repository is bound to organization, as a held event's
// pending binding must be before it is sent.
func boundTo(d routing.Decision, organization string) bool {
	return d.Authority.Kind == routing.Bound && d.Authority.Organization == organization
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
