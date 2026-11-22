package relay

import (
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const traceTTL = time.Hour

// maxHeld and maxHeldBytes bound the hold; when full the oldest parts are evicted, unnamed traces first.
const (
	maxHeld      = 50000
	maxHeldBytes = 64 << 20
)

const maxTraces = 100000

type heldPart struct {
	p    *part
	at   time.Time
	size int
	// pinned is the claim the part was last found claimed under while its team's policy
	// could not be read, and pinnedHow how it was placed: the claim file (claim.TTL) and the
	// trace index (traceTTL) expire long before the part may, and the part stays the team's.
	pinned    *claim.Claim
	pinnedHow attribution
	// awaitingPolicy is that it was last held for its team's policy (whyNoKey), pinned or
	// not: in global mode the catch-all's part has no claim to pin it to.
	awaitingPolicy bool
}

type traceSession struct {
	session string
	// claimed is whether a Claimed session key named it, which its unnamed spans inherit.
	claimed bool
	at      time.Time
}

func (r *Relay) hold(p *part, pin *claim.Claim, how attribution, awaitingPolicy bool) {
	size := proto.Size(p.msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	refuse := func() {
		r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
		if pin != nil || awaitingPolicy {
			r.heldLost += p.records // a team's part, lost: counted where doctor reads losses
		}
	}
	if p.records > maxHeld || size > maxHeldBytes {
		refuse()
		return
	}
	// Evict the oldest rather than refuse the newcomer, the likeliest to be claimed soon.
	for r.heldN+p.records > maxHeld || r.heldBytes+size > maxHeldBytes {
		if !r.evictOldestLocked() {
			refuse()
			return
		}
	}
	r.held[p.session] = append(r.held[p.session], heldPart{p: p, at: r.opts.Now(), size: size, pinned: pin, pinnedHow: how, awaitingPolicy: awaitingPolicy})
	r.heldN += p.records
	r.heldBytes += size
	r.stats.add("held_parts", 1)
}

// evictOldestLocked drops the oldest held part, of an unnamed key first since those are
// rarely ever named. r.mu is held.
func (r *Relay) evictOldestLocked() bool {
	var victim string
	var at time.Time
	for _, traceOnly := range []bool{true, false} {
		for key, parts := range r.held {
			unnamed := strings.HasPrefix(key, tracePrefix) || strings.HasPrefix(key, procPrefix)
			if len(parts) == 0 || (traceOnly && !unnamed) {
				continue
			}
			if victim == "" || parts[0].at.Before(at) {
				victim, at = key, parts[0].at
			}
		}
		if victim != "" {
			break
		}
	}
	if victim == "" {
		return false
	}
	h := r.held[victim][0]
	r.held[victim] = r.held[victim][1:]
	if len(r.held[victim]) == 0 {
		delete(r.held, victim)
	}
	r.heldN -= h.p.records
	r.heldBytes -= h.size
	if h.pinned != nil || h.awaitingPolicy {
		// A team's part, lost: doctor's outbox row counts it (the next sweep records it).
		r.heldLost += h.p.records
	}
	r.stats.dropped(h.p.signal, "unclaimed_evicted", h.p.records)
	return true
}

func (r *Relay) sweep() {
	now := r.opts.Now()
	r.mu.Lock()
	keys := make([]string, 0, len(r.held))
	for k := range r.held {
		keys = append(keys, k)
	}
	for id, t := range r.traces {
		if now.Sub(t.at) >= traceTTL {
			delete(r.traces, id)
		}
	}
	r.mu.Unlock()
	r.watchProcesses(now)
	r.cache.expire(now)

	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	// A team's parts the hold dropped, for age or room, are counted where doctor reads them.
	r.mu.Lock()
	lost := r.heldLost
	r.heldLost = 0
	r.mu.Unlock()
	defer func() { r.outbox.recordHeldLosses(lost, now) }()
	for _, key := range keys {
		hold := r.opts.Hold
		if strings.HasPrefix(key, tracePrefix) || strings.HasPrefix(key, procPrefix) {
			hold = r.opts.TraceHold
		}
		r.mu.Lock()
		parts := r.held[key]
		r.mu.Unlock()
		var keep []heldPart
		type release struct {
			c   claim.Claim
			pol Policy
			p   *part
			how attribution
		}
		var out []release
		// A part that must wait holds up nothing: order matters only among parts that leave.
		for _, h := range parts {
			var c claim.Claim
			var pol Policy
			var why string
			var ok bool
			var how attribution
			if h.pinned != nil && (strings.HasPrefix(key, procPrefix) ||
				r.sessionFor(key) == "" && h.pinnedHow.how == semconv.TermaRelayAttributionProcess) {
				// Placed by its process, pinned only once that process had exited: whatever
				// its pid names since is another process, so the pin alone places it (global
				// mode's catch-all included). A trace named since goes by its session.
				c, pol, why, ok = r.placePinned(*h.pinned, h.p.narrow)
				how = h.pinnedHow
			} else {
				c, pol, why, ok, how = r.decide(key, h.p.pid, h.p.at, h.p.narrow, h.p.claimed)
			}
			if why == whyNoKey && c.ProjectID != "" {
				pinned := c
				h.pinned, h.pinnedHow = &pinned, how
			}
			h.awaitingPolicy = why == whyNoKey
			if !ok && h.pinned != nil && (why == whyUnclaimed || why == whyNoTrace || why == whyProcessIdle) {
				// What placed it (its claim, its trace, its process) expired while the part
				// waited for the team's policy: still placed by that claim, as decideClaimed
				// would. Any other answer (not collected, widened, ambiguous) is a decision,
				// never overridden by a guess.
				c, pol, why, ok = r.placePinned(*h.pinned, h.p.narrow)
				how = h.pinnedHow
			}
			limit := hold
			switch {
			case h.p.start && why == whyUnclaimed:
				// A conversation start waits for the thread's first turn.
				limit = max(hold, r.opts.TraceHold)
			case why == whyNoKey:
				// Claimed for a team whose policy cannot be read now (a refresh the sign-in's
				// remediation blocks): kept as long as the outbox keeps parts, within the
				// hold's bounds, and placed once the policy is back.
				limit = max(hold, maxOutboxAge)
			}
			if !ok && !h.p.narrow && !h.p.claimed && why != whyNoKey && now.Sub(h.at) >= limit {
				if cc, cok := r.catchAll(); cok {
					if cpol, pok := r.resolve(cc); pok && !cpol.RequireClaim {
						c, pol, ok, how = cc, cpol, true, attribution{how: semconv.TermaRelayAttributionCatchAll}
						r.stats.add("caught_by_default."+string(h.p.signal), h.p.records)
					}
				}
			}
			switch {
			case ok:
				out = append(out, release{c, pol, h.p, how})
			case why == whyWidened || why == whyNotCollected || now.Sub(h.at) >= limit:
				r.stats.dropped(h.p.signal, why, h.p.records)
				if (h.pinned != nil || h.awaitingPolicy) && why != whyWidened && why != whyNotCollected {
					lost += h.p.records
				}
				if r.opts.Logf != nil {
					c, _ := r.lookup(r.sessionFor(key))
					r.opts.Logf("drop %s %s: %d %s from pid %d; claim %q pids %v", why, key, h.p.records, h.p.signal, h.p.pid, c.ProjectID, c.PIDs)
				}
			default:
				keep = append(keep, h)
			}
		}
		// hold runs only under deliverMu, which the sweep has.
		r.mu.Lock()
		for _, h := range parts {
			r.heldN -= h.p.records
			r.heldBytes -= h.size
		}
		for _, h := range keep {
			r.heldN += h.p.records
			r.heldBytes += h.size
		}
		if len(keep) == 0 {
			delete(r.held, key)
		} else {
			r.held[key] = keep
		}
		r.mu.Unlock()
		for _, rel := range out {
			r.stats.add("released_after_hold", rel.p.records)
			r.deliverAttributed(rel.c, rel.pol, rel.p, rel.how)
		}
	}
}

func (r *Relay) learnTrace(traceID, session string, claimed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.traces[traceID]; !known && len(r.traces) >= maxTraces {
		r.stats.add("trace_index_full", 1)
		return
	}
	r.traces[traceID] = traceSession{session, claimed, r.opts.Now()}
}

func (r *Relay) traceOf(traceID string) (session string, claimed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.traces[traceID]
	return t.session, t.claimed
}

// sessionFor resolves a held key to a session ("" for a trace not yet named).
func (r *Relay) sessionFor(key string) string {
	if id, ok := strings.CutPrefix(key, tracePrefix); ok {
		session, _ := r.traceOf(id)
		return session
	}
	return key
}
