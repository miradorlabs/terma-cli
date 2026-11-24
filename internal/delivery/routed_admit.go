package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	return r.withhold(team.Policy, e)
}

func sameHost(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

const (
	noDestImportPrefix = "nodest-"
	noDestPruneMarker  = ".nodest-pruned"
	noDestPruneEvery   = time.Hour
)

// recordNoDestination persists each destination-less event's no_destination loss
// atomically under a stable per-event ImportLosses ID (nodest-<queueFileID>-<sha256>-<occ>)
// with deterministic timestamp. Because hooks append with O_APPEND (preserving the queue
// file's OS identity) while spool.ackBatch atomically renames a fresh file (changing it),
// crash-before-ack retries deduplicate even with intervening appends, while byte-identical
// events in later batches or later flushes receive distinct identities and never undercount.
func (p *pass) recordNoDestination(ctx context.Context, noDest []spool.Event) error {
	occ := make(map[string]int, len(noDest))
	now := p.r.currentTime().UTC()
	queueID := ""
	if p.r.StateDir != "" {
		queueID = fileIdentity(filepath.Join(p.r.StateDir, spool.Dir, spool.EventsFile))
	}
	for _, e := range noDest {
		fp := eventFingerprint(e)
		id := fmt.Sprintf("%s%s-%d", noDestImportPrefix, fp, occ[fp])
		if queueID != "" {
			id = fmt.Sprintf("%s%s-%s-%d", noDestImportPrefix, queueID, fp, occ[fp])
		}
		occ[fp]++
		at := e.Time
		if at.IsZero() {
			at = now.Truncate(time.Second)
		}
		if err := p.importNoDestLoss(id, at); err != nil {
			return err
		}
	}
	p.pruneNoDestLosses(ctx, now)
	return nil
}

func (p *pass) importNoDestLoss(id string, at time.Time) error {
	f, err := os.CreateTemp("", ".nodest-loss-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := f.Write(spool.EncodeLoss(LossNoDestination, 1, at)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return p.s.ImportLosses(tmpPath, id)
}

// pruneNoDestLosses removes expired imported~nodest-*.jsonl loss files past
// spool.LossWindow+24h. It runs at most once per pass and at most once per hour
// (throttled via .nodest-pruned) and stops immediately on ctx cancellation so large
// backlogs stay O(N) rather than scanning on every batch.
func (p *pass) pruneNoDestLosses(ctx context.Context, now time.Time) {
	if p.prunedNoDest || p.r.StateDir == "" || ctx.Err() != nil {
		return
	}
	p.prunedNoDest = true
	dir := filepath.Join(p.r.StateDir, spool.Dir, "losses")
	marker := filepath.Join(dir, noDestPruneMarker)
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < noDestPruneEvery {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	const prefix = "imported~" + noDestImportPrefix
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) > spool.LossWindow+24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.WriteFile(marker, nil, 0o600)
}

func eventFingerprint(e spool.Event) string {
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
