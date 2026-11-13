package relay

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// teamSender delivers one enrollment's queue for one tool, as sender delivers a route's.
// Each part is rechecked (Router.Recheck) as it comes up: sent only to the team it was
// routed to while that team takes it, kept while the team may recover, and dropped
// otherwise. A part kept waiting holds up none behind it that may leave.
type teamSender struct {
	r     *Relay
	route teamRoute
	wake  chan struct{}

	mu sync.Mutex
	// queue is the route's parts in arrival order, with their destinations.
	queue []teamEntry
	// waiting: every part left must wait, so the route keeps no relay awake.
	waiting bool
}

func (r *Relay) teamSender(rt teamRoute) *teamSender {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.teamSenders[rt]
	if s == nil {
		s = &teamSender{r: r, route: rt, wake: make(chan struct{}, 1)}
		r.teamSenders[rt] = s
		r.wg.Add(1)
		go s.loop()
	}
	return s
}

// enqueueTeam writes a part to its enrollment's queue before the export is answered, as
// enqueue does. It runs under deliverMu.
func (r *Relay) enqueueTeam(q Queued, tool string, p *part) {
	rt := teamRoute{enrollment: q.Enrollment, tool: tool}
	if rt.tool == "" {
		rt.tool = noTool
	}
	body, err := proto.Marshal(p.msg)
	if err != nil {
		r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	e := newEntry(r.opts.Now(), p.signal, p.records)
	if err := r.outbox.putTeam(rt, q, e, body); err != nil {
		r.stats.dropped(p.signal, "outbox_write_failed", p.records)
		r.warnf("outbox %s: %v; dropped %d %s", rt, err, p.records, p.signal)
		return
	}
	s := r.teamSender(rt)
	s.push(teamEntry{e, q})
	s.poke()
}

func (s *teamSender) push(e teamEntry) {
	s.mu.Lock()
	s.queue = append(s.queue, e)
	s.waiting = false
	s.mu.Unlock()
}

func (s *teamSender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy ignores a route whose every part waits, since only its team recovering moves it.
func (s *teamSender) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) > 0 && !s.waiting
}

// recover queues what a previous relay left, as sender.recover does.
func (s *teamSender) recover(entries []teamEntry) []teamEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	known := map[string]bool{}
	for _, e := range s.queue {
		known[e.name] = true
	}
	var added []teamEntry
	for _, e := range entries {
		if !known[e.name] {
			s.queue = append(s.queue, e)
			added = append(added, e)
		}
	}
	slices.SortFunc(s.queue, func(a, b teamEntry) int { return strings.Compare(a.name, b.name) })
	return added
}

// forget takes parts that left the outbox off the queue.
func (s *teamSender) forget(gone []entry) {
	if len(gone) == 0 {
		return
	}
	names := make(map[string]bool, len(gone))
	for _, e := range gone {
		names[e.name] = true
	}
	s.mu.Lock()
	s.queue = slices.DeleteFunc(s.queue, func(e teamEntry) bool { return names[e.name] })
	if len(s.queue) == 0 {
		s.queue = nil
	}
	s.mu.Unlock()
}

// discard drops parts from the outbox and the queue, counting them under why and their
// blocker.
func (s *teamSender) discard(gone []teamEntry, why, blocker string) {
	if len(gone) == 0 {
		return
	}
	es := make([]entry, len(gone))
	for i, e := range gone {
		es[i] = e.entry
		s.r.stats.dropped(e.signal, why, e.records)
		s.r.block(blocker, e.records)
	}
	s.r.outbox.removeTeam(s.route, es)
	s.forget(es)
}

// take removes from the queue the parts match selects, and returns them.
func (s *teamSender) take(match func(teamEntry) bool) []teamEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []teamEntry
	s.queue = slices.DeleteFunc(s.queue, func(e teamEntry) bool {
		if match(e) {
			out = append(out, e)
			return true
		}
		return false
	})
	return out
}

// dropRun drops session's parts made from from on: the run became Mixed then, and every
// part is telemetry, which carries content (runs.Locational).
func (s *teamSender) dropRun(session string, from time.Time) {
	gone := s.take(func(e teamEntry) bool {
		return e.q.Session == session && (e.q.At.IsZero() || !e.q.At.Before(from))
	})
	s.discard(gone, whyMixed, "mixed:"+session)
}

// choose rechecks the queue in order: it drops what may never leave, skips what must wait,
// and returns up to limit parts that may leave now, of the signal of the first of them,
// with whether any part waits.
func (s *teamSender) choose(limit int) ([]teamEntry, bool) {
	s.mu.Lock()
	queue := slices.Clone(s.queue)
	s.mu.Unlock()
	var batch []teamEntry
	waiting := false
	for _, e := range queue {
		if len(batch) >= limit {
			break
		}
		if len(batch) > 0 && e.signal != batch[0].signal {
			continue
		}
		switch v := s.r.opts.Router.Recheck(e.q); v.Verdict {
		case Discard:
			s.discard([]teamEntry{e}, v.Why.reason(), v.Blocker)
		case Wait:
			waiting = true
		case SendNow:
			batch = append(batch, e)
		}
	}
	s.mu.Lock()
	s.waiting = len(batch) == 0 && waiting
	s.mu.Unlock()
	return batch, waiting
}

// dropAll drops the whole queue, as for an enrollment that ended.
func (s *teamSender) dropAll(why string) {
	s.discard(s.take(func(teamEntry) bool { return true }), why, "")
}

// loop delivers the queue, as sender.loop does a route's.
func (s *teamSender) loop() {
	defer s.r.wg.Done()
	ctx := s.r.sendCtx
	backoff := time.Duration(0)
	single := false
	stopping := func() bool {
		select {
		case <-s.r.stopping:
			return true
		default:
			return false
		}
	}
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		wake := s.wake
		if backoff > 0 {
			wake = nil
		}
		stop := s.r.stopping
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
				return false
			}
		}
		return true
	}
	for ctx.Err() == nil {
		limit := maxMergeFiles
		if single {
			limit = 1
		}
		batch, waiting := s.choose(limit)
		if len(batch) == 0 {
			wait := idleWait
			if waiting {
				wait = keylessRetry
			}
			if stopping() || !sleep(wait) {
				return
			}
			continue
		}
		pol, ok := s.r.resolveTeam(s.route.enrollment, s.route.toolLabel())
		if !ok {
			// Its key is minted, or the keychain locked: the parts wait.
			s.mu.Lock()
			s.waiting = true
			s.mu.Unlock()
			if stopping() || !sleep(keylessRetry) {
				return
			}
			continue
		}
		sig := batch[0].signal
		var bodies [][]byte
		var taken []entry
		size := 0
		for _, e := range batch {
			b, err := s.r.outbox.readTeam(s.route, e.entry)
			if err != nil {
				if len(taken) == 0 {
					s.r.stats.dropped(e.signal, "outbox_unreadable", e.records)
					s.r.outbox.buryTeam(s.route, e.entry)
					s.forget([]entry{e.entry})
				}
				break
			}
			if len(taken) > 0 && size+len(b) > maxMergeBytes {
				break
			}
			bodies, taken, size = append(bodies, b), append(taken, e.entry), size+len(b)
		}
		if len(taken) == 0 {
			continue
		}
		body := bodies[0]
		if len(bodies) > 1 {
			merged, err := mergeBodies(sig, bodies)
			if err != nil {
				taken, merged = taken[:1], bodies[0]
			}
			body = merged
		}
		records := 0
		for _, e := range taken {
			records += e.records
		}
		// The team's content policy as it stands now.
		body, tl := s.r.withholdQueued(sig, body, pol)
		if pol.Signals != nil && !contains(pol.Signals, string(sig)) || body == nil {
			s.r.stats.dropped(sig, "policy_signal_or_content", records)
			s.r.outbox.removeTeam(s.route, taken)
			s.forget(taken)
			continue
		}
		out, wait, detail, rejected := s.r.post(ctx, pol, sig, body)
		switch out {
		case sent:
			s.r.count(sig, tl)
			s.r.stats.forwarded(sig, records)
			if rejected > 0 {
				s.r.stats.add("upstream_rejected."+string(sig), int(rejected))
			}
			s.r.outbox.removeTeam(s.route, taken)
			s.forget(taken)
			single, backoff = false, 0
		case refused:
			if len(taken) > 1 {
				single = true
				continue
			}
			single = false
			s.r.stats.dropped(sig, "upstream_"+detail, taken[0].records)
			s.r.warnf("upstream %s refused %d %s: %s", s.route, taken[0].records, sig, detail)
			s.r.outbox.buryTeam(s.route, taken[0])
			s.forget(taken[:1])
		case retry:
			if ctx.Err() != nil {
				return
			}
			s.r.stats.add("upstream_retries", 1)
			s.r.warnf("upstream %s: %s; retrying", s.route, detail)
			backoff = nextBackoff(backoff, wait)
			if !sleep(jitter(backoff, wait)) {
				return
			}
		}
	}
}

// dropMixed drops, from every enrollment's queue, session's parts made from from on, once
// for each earlier Mixed time learned.
func (r *Relay) dropMixed(session string, from time.Time) {
	r.mu.Lock()
	if seen, ok := r.mixedSeen[session]; ok && !from.Before(seen) {
		r.mu.Unlock()
		return
	}
	if len(r.mixedSeen) >= maxTraces {
		clear(r.mixedSeen)
	}
	r.mixedSeen[session] = from
	senders := slices.Collect(maps.Values(r.teamSenders))
	r.mu.Unlock()
	for _, s := range senders {
		s.dropRun(session, from)
	}
}

// dropDeselected drops the queue of every enrollment unticked or released since.
func (r *Relay) dropDeselected() {
	if !r.teams() {
		return
	}
	r.mu.Lock()
	senders := slices.Collect(maps.Values(r.teamSenders))
	r.mu.Unlock()
	for _, s := range senders {
		if !r.opts.Router.Selected(s.route.enrollment) {
			s.dropAll(whyDeselected)
		}
	}
}
