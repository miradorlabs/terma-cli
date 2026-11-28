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

	"github.com/miradorlabs/terma-cli/internal/routing"
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
	v, _ := pr.router.Recheck(routing.Destination{EnrollmentID: e.Enrollment, Location: locationOf(e)}, now)
	switch {
	case v == routing.Discard || !ok:
		res.Discarded++
		return routing.Team{}, spool.Event{}, admitDrop
	case v == routing.Wait:
		res.Blocked++
		return routing.Team{}, spool.Event{}, admitHold
	case v != routing.SendNow:
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

const noDestImportPrefix = "nodest-"

// recordNoDestination persists a batch's destination-less events' no_destination losses in
// one ImportLosses file per batch (nodest-<queueFileID>-<firstEventHash>). Because hooks
// append with O_APPEND (preserving both the queue file's OS identity and the first event)
// while spool.ackBatch atomically renames a fresh file (changing it), a crash-before-ack
// retry overwrites the same file with the same leading lines plus any newly appended events,
// keeping both ReadLosses and byte-offset TakeNewLosses exact with O(batches) files instead
// of O(events).
func (p *pass) recordNoDestination(ctx context.Context, noDest []spool.Event) error {
	now := p.r.currentTime().UTC()
	queueID := ""
	if p.r.StateDir != "" {
		queueID = fileIdentity(filepath.Join(p.r.StateDir, spool.Dir, spool.EventsFile))
	}
	id := noDestImportPrefix + batchFingerprint(noDest)
	if queueID != "" {
		id = fmt.Sprintf("%s%s-%s", noDestImportPrefix, queueID, eventFingerprint(noDest[0]))
	}
	if err := p.importNoDestLosses(id, noDest, now); err != nil {
		return err
	}
	if !p.prunedNoDest && ctx.Err() == nil {
		p.prunedNoDest = true
		p.s.PruneLosses(now)
	}
	return nil
}

func (p *pass) importNoDestLosses(id string, noDest []spool.Event, now time.Time) error {
	f, err := os.CreateTemp("", ".nodest-loss-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	fallback := now.Truncate(time.Second)
	for _, e := range noDest {
		at := e.Time
		if at.IsZero() {
			at = fallback
		}
		if _, err := f.Write(spool.EncodeLoss(LossNoDestination, 1, at)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return p.s.ImportLosses(tmpPath, id)
}

func eventFingerprint(e spool.Event) string {
	raw, _ := json.Marshal(e)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func batchFingerprint(events []spool.Event) string {
	h := sha256.New()
	for _, e := range events {
		raw, _ := json.Marshal(e)
		h.Write(raw)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
