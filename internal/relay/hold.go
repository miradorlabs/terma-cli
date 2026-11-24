package relay

import (
	"cmp"
	"maps"
	"slices"
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

// policyHold is how long a claimed part waits in the hold for its team's policy or key
// while the relay can't read them (whyNoKey: a policy not yet fetched, which the policy
// refresher fetches meanwhile, a locked keychain, a changed organization, a removed profile,
// an unreadable config.json): past it the part is dropped, and counted as lostPolicyTimeout.
const policyHold = 15 * time.Minute

type heldPart struct {
	p    *part
	at   time.Time
	size int
	// awaitingPolicy is when it began waiting for its team's policy or key (whyNoKey), zero
	// while it waits for anything else: it waits for them policyHold at most.
	awaitingPolicy time.Time
	pin            *pinnedDest // non-nil only in r.pinnedHold; sweeps call Recheck, never Router.Route
}

type pinnedDest struct {
	q            Queued
	c            claim.Claim
	how          attribution
	why, blocker string
}

type traceSession struct {
	session string
	// claimed is whether a Claimed session key named it, which its unnamed spans inherit.
	claimed bool
	at      time.Time
}

func (r *Relay) hasHeld(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.held[key]) > 0 || len(r.pinnedHold[key]) > 0
}
func (r *Relay) hold(p *part, awaitingPolicy bool) { r.holdInto(p, awaitingPolicy, nil) }

func (r *Relay) holdInto(p *part, awaitingPolicy bool, pin *pinnedDest) {
	size := proto.Size(p.msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	refuse := func() {
		r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
		if awaitingPolicy {
			r.policyLost += p.records // a team's part, lost: counted where doctor reads losses
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
	h := heldPart{p: p, at: r.opts.Now(), size: size, pin: pin}
	if awaitingPolicy {
		h.awaitingPolicy = h.at
	}
	dst := map[bool]map[string][]heldPart{true: r.pinnedHold, false: r.held}[pin != nil]
	dst[p.session] = append(dst[p.session], h)
	r.heldN += p.records
	r.heldBytes += size
	r.stats.add("held_parts", 1)
}

// evictOldestLocked drops the oldest held part, of an unnamed key first since those are
// rarely ever named. r.mu is held.
func (r *Relay) evictOldestLocked() bool {
	var victim string
	var at time.Time
	dst := r.held
	for i, m := range []map[string][]heldPart{r.held, r.held, r.pinnedHold} {
		for key, parts := range m {
			unnamed := strings.HasPrefix(key, tracePrefix) || strings.HasPrefix(key, procPrefix)
			if len(parts) == 0 || (i == 0 && !unnamed) {
				continue
			}
			if victim == "" || parts[0].at.Before(at) {
				victim, at, dst = key, parts[0].at, m
			}
		}
		if victim != "" {
			break
		}
	}
	if victim == "" {
		return false
	}
	h := dst[victim][0]
	dst[victim] = dst[victim][1:]
	if len(dst[victim]) == 0 {
		delete(dst, victim)
	}
	r.heldN -= h.p.records
	r.heldBytes -= h.size
	if !h.awaitingPolicy.IsZero() {
		// A team's part, lost: doctor's outbox row counts it (the next sweep records it).
		r.policyLost += h.p.records
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
	pinnedKeys := slices.Collect(maps.Keys(r.pinnedHold))
	for id, t := range r.traces {
		if now.Sub(t.at) >= traceTTL {
			delete(r.traces, id)
		}
	}
	r.mu.Unlock()
	r.watchProcesses(now)
	r.cache.expire(now)
	r.dropDeselected()

	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	// A team's parts the hold dropped waiting for its policy, past policyHold or for room,
	// are counted where doctor reads them.
	r.mu.Lock()
	lost := r.policyLost
	r.policyLost = 0
	r.mu.Unlock()
	defer func() { r.recordLosses(map[string]int{lostPolicyTimeout: lost}, now) }()
	for _, key := range keys {
		hold := r.opts.Hold
		if strings.HasPrefix(key, tracePrefix) || strings.HasPrefix(key, procPrefix) {
			hold = r.opts.TraceHold
		}
		r.mu.Lock()
		parts := r.held[key]
		r.mu.Unlock()
		var keep, pin []heldPart
		var out []verdictPart
		// A part that must wait holds up nothing: order matters only among parts that leave.
		for _, h := range r.expandHeldPieces(key, parts) {
			v := r.judge(key, h.p)
			if before, after := r.splitMixed(h.p, v); before != nil {
				if after != nil {
					r.dropStats(after.signal, v.why, v.blocker, after.records)
				}
				h.p, h.size = before, proto.Size(before.msg)
				v = r.judge(key, h.p)
			}
			why, ok := v.why, v.ok
			switch {
			case why != whyNoKey:
				h.awaitingPolicy = time.Time{}
			case h.awaitingPolicy.IsZero():
				h.awaitingPolicy = now
			}
			limit := hold
			if h.p.start && why == whyUnclaimed {
				// A conversation start waits for the thread's first turn.
				limit = max(hold, r.opts.TraceHold)
			}
			if !ok && !h.p.narrow && !h.p.claimed && why != whyNoKey && now.Sub(h.at) >= limit {
				if cc, cok := r.catchAll(); cok {
					if cpol, pok := r.resolve(cc, h.p.account); pok && !cpol.RequireClaim {
						v, ok = verdict{c: cc, pol: cpol, ok: true, how: attribution{how: semconv.TermaRelayAttributionCatchAll}}, true
						r.stats.add("caught_by_default."+string(h.p.signal), h.p.records)
					}
				}
			}
			switch {
			case ok:
				out = append(out, verdictPart{v, h.p})
			case v.pin != nil:
				pin = append(pin, heldPart{p: h.p, awaitingPolicy: map[bool]time.Time{true: now}[why == whyNoKey || why == string(WhyBlocked) || why == string(WhyNoState)], pin: &pinnedDest{*v.pin, v.c, v.how, why, v.blocker}})
			case !r.teams() && why == whyNoKey && now.Sub(h.awaitingPolicy) < policyHold:
				keep = append(keep, h)
			case why == whyNoKey || why == whyWidened || why == whyNotCollected || v.final || now.Sub(h.at) >= limit:
				if why == whyNoKey {
					// Claimed for a team whose policy or key couldn't be read in time: lost, and counted.
					why = lostPolicyTimeout
					lost += h.p.records
				}
				r.dropStats(h.p.signal, why, v.blocker, h.p.records)
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
		for _, pn := range pin {
			r.holdInto(pn.p, !pn.awaitingPolicy.IsZero(), pn.pin)
		}
		for _, rel := range out {
			r.stats.add("released_after_hold", rel.p.records)
			r.deliver(rel.v, rel.p)
		}
	}
	r.sweepPinned(pinnedKeys, now, &lost)
}

func (r *Relay) expandHeldPieces(key string, parts []heldPart) []heldPart {
	if !r.teams() {
		return parts
	}
	var out []heldPart
	for _, h := range parts {
		for _, pc := range r.pieces(key, h.p) {
			hp := h
			if pc != h.p {
				hp.p, hp.size = pc, proto.Size(pc.msg)
			}
			out = append(out, hp)
		}
	}
	return out
}
func (r *Relay) sweepPinned(keys []string, now time.Time, lost *int) {
	rcs := map[[2]string]Rechecker{}
	for _, key := range keys {
		hold := map[bool]time.Duration{true: r.opts.TraceHold, false: r.opts.Hold}[strings.HasPrefix(key, tracePrefix) || strings.HasPrefix(key, procPrefix)]
		r.mu.Lock()
		parts := r.pinnedHold[key]
		r.mu.Unlock()
		keep, out, dn, db := []heldPart(nil), []verdictPart(nil), 0, 0
		retain := func(h heldPart) { keep, dn, db = append(keep, h), dn+h.p.records, db+h.size }
		for _, h := range parts {
			dn, db = dn-h.p.records, db-h.size
			pn := h.pin
			if !r.teams() || !r.laneActive(h.p.account) {
				if r.teams() && now.Sub(h.at) < hold {
					retain(h)
					continue
				}
				if !h.awaitingPolicy.IsZero() {
					*lost += h.p.records
				}
				r.dropStats(h.p.signal, cmp.Or(pn.why, string(WhyBlocked)), pn.blocker, h.p.records)
				continue
			}
			rk := [2]string{pn.q.Enrollment, h.p.account}
			if rcs[rk] == nil {
				rcs[rk] = r.opts.Router.Recheck(rk[0], rk[1])
			}
			res, waits := rcs[rk].TeamWaits()
			if !waits {
				res = rcs[rk].Entry(pn.q)
			}
			if res.Verdict == Discard && res.Why == WhyMixed && !res.MixedAt.IsZero() && h.p.records > 1 {
				if before, after := splitBeforeMixed(h.p, res.MixedAt); before != nil {
					if after != nil {
						r.dropStats(after.signal, res.Why.reason(), res.Blocker, after.records)
					}
					h.p, h.size, pn.q.At, pn.q.EndAt = before, proto.Size(before.msg), before.at, before.endAt
					res = rcs[rk].Entry(pn.q)
				}
			}
			switch res.Verdict {
			case Discard:
				r.dropStats(h.p.signal, res.Why.reason(), res.Blocker, h.p.records)
				continue
			case SendNow:
				if pol, ok := r.resolveTeam(pn.q.Enrollment, pn.c.Tool, h.p.account, true); ok && r.mayLeaveAccount(h.p.account, pol.Key != "") && pol.resolvedFor(h.p.account) {
					q := pn.q
					out = append(out, verdictPart{verdict{ok: true, pol: pol, c: pn.c, q: &q, how: pn.how}, h.p})
					continue
				}
				pn.why, res.Why = whyNoKey, WhyNoState
			default:
				pn.why, pn.blocker = res.Why.reason(), res.Blocker
			}
			if res.Why == WhyBlocked || res.Why == WhyNoState {
				if h.awaitingPolicy = nonZeroTime(h.awaitingPolicy, now); now.Sub(h.awaitingPolicy) < policyHold {
					retain(h)
					continue
				}
				*lost += h.p.records
				if res.Why == WhyNoState {
					pn.why = lostPolicyTimeout
				}
			} else if now.Sub(h.at) < hold {
				h.awaitingPolicy = time.Time{}
				retain(h)
				continue
			}
			r.dropStats(h.p.signal, pn.why, pn.blocker, h.p.records)
		}
		r.mu.Lock()
		r.heldN, r.heldBytes = r.heldN+dn, r.heldBytes+db
		if len(keep) == 0 {
			delete(r.pinnedHold, key)
		} else {
			r.pinnedHold[key] = keep
		}
		r.mu.Unlock()
		for _, rel := range out {
			r.stats.add("released_after_hold", rel.p.records)
			r.deliver(rel.v, rel.p)
		}
	}
}

type verdictPart struct {
	v verdict
	p *part
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
