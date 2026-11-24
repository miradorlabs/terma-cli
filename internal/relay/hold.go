package relay

import (
	"cmp"
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
}

type traceSession struct {
	session string
	// claimed is whether a Claimed session key named it, which its unnamed spans inherit.
	claimed bool
	at      time.Time
}

// hold keeps p until it may leave; awaitingPolicy is that it waits for its team's policy.
func (r *Relay) hold(p *part, awaitingPolicy bool) {
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
	h := heldPart{p: p, at: r.opts.Now(), size: size}
	if awaitingPolicy {
		h.awaitingPolicy = h.at
	}
	r.held[p.session] = append(r.held[p.session], h)
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
			c, pol, why, ok, how := r.decide(key, h.p.pid, h.p.at, h.p.narrow, h.p.claimed, h.p.account)
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
						c, pol, ok, how = cc, cpol, true, attribution{how: semconv.TermaRelayAttributionCatchAll}
						r.stats.add("caught_by_default."+string(h.p.signal), h.p.records)
					}
				}
			}
			switch {
			case ok:
				out = append(out, release{c, pol, h.p, how})
			case why == whyNoKey && now.Sub(h.awaitingPolicy) < policyHold:
				keep = append(keep, h)
			case why == whyNoKey || why == whyWidened || why == whyNotCollected || now.Sub(h.at) >= limit:
				if why == whyNoKey {
					// Claimed for a team whose policy or key couldn't be read in time: lost, and counted.
					why = lostPolicyTimeout
					lost += h.p.records
				}
				r.stats.dropped(h.p.signal, why, h.p.records)
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

// Waiting is a held key's parts of one signal from one sender, with why they cannot leave
// yet: what the /held endpoint lists, so a test that waited in vain can say what for.
// Reason is empty for parts the next sweep releases.
type Waiting struct {
	Key     string    `json:"key"`
	Session string    `json:"session,omitempty"`
	Signal  Signal    `json:"signal"`
	PID     int       `json:"pid"`
	Reason  string    `json:"reason"`
	Parts   int       `json:"parts"`
	Records int       `json:"records"`
	Oldest  time.Time `json:"oldest"`
	// ClaimPIDs are the processes the session's claim names, when it has one.
	ClaimPIDs []int `json:"claim_pids,omitempty"`
}

// Held lists what the hold keeps, decided as the sweep would decide it now.
func (r *Relay) Held() []Waiting {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	r.mu.Lock()
	held := make(map[string][]heldPart, len(r.held))
	for k, parts := range r.held {
		held[k] = slices.Clone(parts)
	}
	r.mu.Unlock()
	type group struct {
		key    string
		signal Signal
		pid    int
		reason string
	}
	byGroup := map[group]*Waiting{}
	for key, parts := range held {
		session := r.sessionFor(key)
		var claimPIDs []int
		if session != "" {
			if c, ok := r.lookup(session); ok {
				claimPIDs = c.PIDs
			}
		}
		for _, h := range parts {
			_, _, why, _, _ := r.decide(key, h.p.pid, h.p.at, h.p.narrow, h.p.claimed)
			g := group{key, h.p.signal, h.p.pid, why}
			w := byGroup[g]
			if w == nil {
				w = &Waiting{Key: key, Session: session, Signal: h.p.signal, PID: h.p.pid, Reason: why, Oldest: h.at, ClaimPIDs: claimPIDs}
				byGroup[g] = w
			}
			w.Parts++
			w.Records += h.p.records
			if h.at.Before(w.Oldest) {
				w.Oldest = h.at
			}
		}
	}
	out := make([]Waiting, 0, len(byGroup))
	for _, w := range byGroup {
		out = append(out, *w)
	}
	slices.SortFunc(out, func(a, b Waiting) int {
		return cmp.Or(strings.Compare(a.Key, b.Key), strings.Compare(string(a.Signal), string(b.Signal)), cmp.Compare(a.PID, b.PID), strings.Compare(a.Reason, b.Reason))
	})
	return out
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
