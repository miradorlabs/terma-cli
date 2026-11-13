package relay

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const (
	sendTimeout   = 15 * time.Second
	maxMergeBytes = 4 << 20
	maxMergeFiles = 64
	minBackoff    = time.Second
	maxBackoff    = 2 * time.Minute
	maxRetryAfter = 10 * time.Minute
	// A refused key is not fixed by asking again soon: `terma setup` stores a new one.
	refusedBackoff    = 5 * time.Minute
	maxRefusedBackoff = time.Hour
	keylessRetry      = time.Minute
	idleWait          = 30 * time.Second
)

// sender keeps one route's outbox, for drain to send, so one refusing host never holds up
// another project.
type sender struct {
	r     *Relay
	route route
	wake  chan struct{}

	mu sync.Mutex
	// queue is the route's outbox in arrival order, one queue per signal, so the sender never
	// lists the directory again after recoverOutbox: a backed-up route drains in linear time.
	queue   map[Signal][]entry
	keyless bool // a keyless route waits and keeps no relay awake

	// c is the route's claim, as its outbox names it: read once, when placed, by the
	// goroutine that drains the route alone.
	c      claim.Claim
	placed bool
}

func (r *Relay) sender(rt route) *sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.senders[rt]
	if s == nil {
		s = &sender{r: r, route: rt, wake: make(chan struct{}, 1), queue: map[Signal][]entry{}}
		r.senders[rt] = s
		r.wg.Add(1)
		go r.drain(s, s.wake)
	}
	return s
}

// enqueue writes a part to its route's outbox before the export is answered, so a crash
// loses none. It runs under deliverMu, so files are named in arrival order.
func (r *Relay) enqueue(c claim.Claim, p *part) {
	rt := routeOf(c)
	body, err := proto.Marshal(p.msg)
	if err != nil {
		r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	e := newEntry(r.opts.Now(), p.signal, p.records)
	if err := r.outbox.put(rt, c.Repository, e, body); err != nil {
		r.stats.dropped(p.signal, "outbox_write_failed", p.records)
		r.warnf("outbox %s: %v; dropped %d %s", rt, err, p.records, p.signal)
		return
	}
	s := r.sender(rt)
	s.push(e)
	s.poke()
}

func (s *sender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy ignores a keyless route, since only a new key can move it.
func (s *sender) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queue {
		if len(q) > 0 {
			return !s.keyless
		}
	}
	return false
}

func (s *sender) setKeyless(v bool) {
	s.mu.Lock()
	s.keyless = v
	s.mu.Unlock()
}

// sendQueue is one destination's queue as drain sends it: a route's (sender) or an
// enrollment's (teamSender). Each keeps its own queue, and says what may leave and under
// which policy; drain sends, retries and backs off the same way for both.
type sendQueue interface {
	// batch is the next parts that may leave now, oldest first: up to limit, of one
	// signal; and whether any part left waits, which only its team recovering moves.
	batch(limit int) ([]entry, bool)
	// policy is the send policy now: ok false while the parts wait for it, and discard
	// naming why they are dropped instead.
	policy() (pol Policy, ok bool, discard string)
	// read is a queued part's body.
	read(e entry) ([]byte, error)
	// remove takes parts, sent or dropped, out of the outbox and the queue.
	remove(batch []entry)
	// bury sets a part aside with the dead letters, and takes it off the queue.
	bury(e entry)
	fmt.Stringer
}

// drain sends q oldest first, woken by wake when a part arrives; a stopping relay keeps
// going within its grace. Each sender runs it on its own goroutine, so one refusing host
// never holds up another destination.
func (r *Relay) drain(q sendQueue, wake <-chan struct{}) {
	defer r.wg.Done()
	ctx := r.sendCtx
	backoff := time.Duration(0)
	// single sends one file per request after a merged request was refused, so one bad file is set aside alone.
	single := false
	stopping := func() bool {
		select {
		case <-r.stopping:
			return true
		default:
			return false
		}
	}
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		wake := wake
		if backoff > 0 {
			wake = nil // new work does not cut a backoff short: the host is the same
		}
		stop := r.stopping
		if stopping() {
			stop = nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		case <-wake:
		case <-stop:
			if backoff > 0 {
				return false // a failing host is not retried on the way out
			}
		}
		return true
	}
	for ctx.Err() == nil {
		limit := maxMergeFiles
		if single {
			limit = 1
		}
		entries, waiting := q.batch(limit)
		if len(entries) == 0 {
			wait := idleWait
			if waiting {
				wait = keylessRetry
			}
			if stopping() || !sleep(wait) {
				return
			}
			continue
		}
		pol, ok, discard := q.policy()
		if discard != "" {
			for _, e := range entries {
				r.stats.dropped(e.signal, discard, e.records)
			}
			q.remove(entries)
			continue
		}
		if !ok {
			if stopping() || !sleep(keylessRetry) {
				return
			}
			continue
		}
		batch, body := r.take(q, entries)
		if len(batch) == 0 {
			continue
		}
		sig, records := batch[0].signal, 0
		for _, e := range batch {
			records += e.records
		}
		// The content policy as it stands now: a team that turned prompts off since sends none still queued.
		// Counted only once the batch leaves the outbox: a retry filters it again.
		body, tl := r.withholdQueued(sig, body, pol)
		if pol.Signals != nil && !contains(pol.Signals, string(sig)) || body == nil {
			r.stats.dropped(sig, "policy_signal_or_content", records)
			q.remove(batch)
			continue
		}
		out, wait, detail, rejected := r.post(ctx, pol, sig, body)
		switch out {
		case sent:
			r.count(sig, tl)
			r.stats.forwarded(sig, records)
			if rejected > 0 {
				// Resending a partial success would only duplicate what was kept.
				r.stats.add("upstream_rejected."+string(sig), int(rejected))
			}
			q.remove(batch)
			single, backoff = false, 0
		case refused:
			if len(batch) > 1 {
				single = true
				continue
			}
			single = false
			r.stats.dropped(sig, "upstream_"+detail, batch[0].records)
			r.warnf("upstream %s refused %d %s: %s", q, batch[0].records, sig, detail)
			q.bury(batch[0])
		case retry:
			if ctx.Err() != nil {
				return
			}
			r.stats.add("upstream_retries", 1)
			r.warnf("upstream %s: %s; retrying", q, detail)
			backoff = nextBackoff(backoff, wait)
			if !sleep(jitter(backoff, wait)) {
				return
			}
		}
	}
}

// take reads entries, of one signal, into one request: as many as fit maxMergeBytes,
// merged, and the parts it holds. A first part that cannot be read never will be, so it
// is buried rather than hold back the rest, and take returns none.
func (r *Relay) take(q sendQueue, entries []entry) ([]entry, []byte) {
	var batch []entry
	var bodies [][]byte
	size := 0
	for _, e := range entries {
		b, err := q.read(e)
		if err != nil {
			if len(batch) == 0 {
				r.stats.dropped(e.signal, "outbox_unreadable", e.records)
				q.bury(e)
			}
			break
		}
		if len(batch) > 0 && size+len(b) > maxMergeBytes {
			break
		}
		batch, bodies, size = append(batch, e), append(bodies, b), size+len(b)
	}
	if len(batch) <= 1 {
		if len(batch) == 0 {
			return nil, nil
		}
		return batch, bodies[0]
	}
	merged, err := mergeBodies(batch[0].signal, bodies)
	if err != nil {
		return batch[:1], bodies[0]
	}
	return batch, merged
}

// batch is the route's next parts (next); none of them waits.
func (s *sender) batch(limit int) ([]entry, bool) { return s.next(limit), false }

func (s *sender) read(e entry) ([]byte, error) { return s.r.outbox.read(s.route, e) }

func (s *sender) remove(batch []entry) {
	s.r.outbox.remove(s.route, batch)
	s.forget(batch)
}

func (s *sender) bury(e entry) {
	s.r.outbox.bury(s.route, e)
	s.forget([]entry{e})
}

func (s *sender) String() string { return s.route.String() }

type outcome int

const (
	sent outcome = iota
	retry
	refused
)

// policy is the route's send policy now; ok false while it must wait, which keeps no
// relay awake (keyless), and discard names why its parts are dropped: among them a
// repository the list no longer admits.
func (s *sender) policy() (Policy, bool, string) {
	if !s.placed {
		s.c = claim.Claim{ProjectID: s.route.project, Tool: s.route.toolLabel(), Repository: s.r.outbox.identity(s.route)}
		s.placed = true
	}
	pol, ok, discard := s.policyFor(s.c)
	switch {
	case discard != "":
		return Policy{}, false, discard
	case !ok:
		s.setKeyless(true)
		return Policy{}, false, ""
	}
	s.setKeyless(false)
	// The repository list as it stands now: a repository removed since sends none still queued.
	if pol.Unadmitted {
		return Policy{}, false, "policy_repository"
	}
	return pol, true, ""
}

// policyFor is the policy of c, the route's claim, now. On a migrated machine a route
// queued before the migration is its project's cutover enrollment's (Router.Legacy),
// rechecked as that team's, and dropped where the project maps to none: records from
// before the migration keep their project, and never reach another team. The relay
// migrates the machine while it runs, so a route can be queued under one rule and sent
// under the other.
func (s *sender) policyFor(c claim.Claim) (pol Policy, ok bool, discard string) {
	if !s.r.teams() {
		pol, ok = s.r.resolve(c)
		return pol, ok, ""
	}
	q, mapped := s.r.opts.Router.Legacy(c.ProjectID, c.Repository)
	switch {
	case !mapped:
		return Policy{}, false, whyLegacy
	case q.Enrollment == "":
		// The routing state cannot be read yet, as while the migration commits: it waits.
		return Policy{}, false, ""
	}
	switch v := s.r.opts.Router.Recheck(q.Enrollment).Entry(q); v.Verdict {
	case Discard:
		return Policy{}, false, v.Why.reason()
	case Wait:
		return Policy{}, false, ""
	}
	pol, ok = s.r.resolveTeam(q.Enrollment, c.Tool)
	return pol, ok, ""
}

func nextBackoff(prev, floor time.Duration) time.Duration {
	next := max(minBackoff, prev*2, floor)
	limit := maxBackoff
	if floor >= refusedBackoff {
		limit = maxRefusedBackoff
	}
	return min(next, max(limit, floor))
}

// jitter spreads a wait over ±20% so relays that lost the gateway together do not return together.
func jitter(d, floor time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) * 2 / 5
	return max(floor, d-time.Duration(spread/2)+time.Duration(rand.Int64N(spread+1)))
}

func (r *Relay) recoverOutbox() {
	routes, _ := r.outbox.routes()
	for _, rt := range routes {
		r.recoverRoute(rt)
	}
	teams, _ := r.outbox.teamRoutes()
	for _, rt := range teams {
		r.recoverTeamRoute(rt)
	}
}

// recoverTeamRoute is recoverRoute for an enrollment's queue.
func (r *Relay) recoverTeamRoute(rt teamRoute) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	entries, err := r.outbox.listTeam(rt)
	if err != nil || len(entries) == 0 {
		return
	}
	s := r.teamSender(rt)
	n := 0
	for _, e := range s.recover(entries) {
		n += e.records
	}
	r.stats.add("recovered_from_outbox", n)
	s.poke()
}

// recoverRoute queues what a previous relay left on rt. It holds deliverMu so an export
// arriving meanwhile is never between writing its part and queueing it: the part would be
// listed here, then pushed again, and sent twice.
func (r *Relay) recoverRoute(rt route) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	entries, err := r.outbox.list(rt)
	if err != nil || len(entries) == 0 {
		return
	}
	s := r.sender(rt)
	n := 0
	for _, e := range s.recover(entries) {
		n += e.records
	}
	r.stats.add("recovered_from_outbox", n)
	s.poke()
}
