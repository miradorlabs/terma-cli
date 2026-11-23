package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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

// noDestSeenFile holds per-event fingerprints of destination-less events already recorded
// via CountLost in an unacknowledged batch, preventing double-counting across crashes or
// canceled ackBatch calls even when hooks append new events before the retry. Deleted as
// soon as spool.Flush acknowledges the batch.
const noDestSeenFile = "nodest-losses-pending.json"

type noDestJournal struct {
	Seen map[string]time.Time `json:"seen"`
}

func (p *pass) noDestSeenPath() string {
	if p.r.StateDir == "" {
		return ""
	}
	return filepath.Join(p.r.StateDir, noDestSeenFile)
}

// recordNoDestination records no_destination losses via Spool.CountLost (which appends to
// the daily loss log and runs standard retention pruning) while deduplicating per event
// against any prior unacknowledged batch recorded in nodest-losses-pending.json.
func (p *pass) recordNoDestination(noDest []spool.Event) error {
	now := p.r.currentTime().UTC()
	seen := p.loadPendingNoDest(now)
	occ := make(map[string]int, len(noDest))
	keys := make([]string, 0, len(noDest))
	newCount := 0
	for _, e := range noDest {
		fp := eventFingerprint(e)
		key := fmt.Sprintf("%s#%d", fp, occ[fp])
		occ[fp]++
		keys = append(keys, key)
		if _, already := seen[key]; !already {
			newCount++
		}
	}
	if newCount > 0 {
		if err := p.s.CountLost(LossNoDestination, newCount); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if _, already := seen[k]; !already {
			seen[k] = now
		}
	}
	p.savePendingNoDest(seen)
	p.lastNoDestKeys = keys
	return nil
}

func (p *pass) ackPendingNoDest(keys []string) {
	if p.noDestSeenPath() == "" || len(keys) == 0 {
		return
	}
	seen := p.loadPendingNoDest(p.r.currentTime().UTC())
	if len(seen) == 0 {
		return
	}
	for _, k := range keys {
		delete(seen, k)
	}
	p.savePendingNoDest(seen)
}

func (p *pass) finishNoDestination(fr spool.Result) {
	if !p.res.Routed || fr.Skipped {
		return
	}
	if fr.Err == nil {
		if path := p.noDestSeenPath(); path != "" {
			_ = os.Remove(path)
		}
		p.lastNoDestKeys = nil
		return
	}
	if len(p.lastNoDestKeys) > 0 && fr.Sent == p.expectedAck {
		p.ackPendingNoDest(p.lastNoDestKeys)
		p.lastNoDestKeys = nil
	}
}

func (p *pass) loadPendingNoDest(now time.Time) map[string]time.Time {
	seen := map[string]time.Time{}
	path := p.noDestSeenPath()
	if path == "" {
		return seen
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return seen
	}
	var j noDestJournal
	if json.Unmarshal(raw, &j) != nil {
		return seen
	}
	for k, ts := range j.Seen {
		if now.Sub(ts) <= spool.MaxAge {
			seen[k] = ts
		}
	}
	return seen
}

func (p *pass) savePendingNoDest(seen map[string]time.Time) {
	path := p.noDestSeenPath()
	if path == "" {
		return
	}
	if len(seen) == 0 {
		_ = os.Remove(path)
		return
	}
	raw, err := json.Marshal(noDestJournal{Seen: seen})
	if err != nil {
		return
	}
	_ = os.MkdirAll(p.r.StateDir, 0o700)
	_ = config.WriteFileAtomic(path, raw, 0o600)
}

func eventFingerprint(e spool.Event) string {
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
