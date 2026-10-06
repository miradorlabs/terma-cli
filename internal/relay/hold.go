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
}

type traceSession struct {
	session string
	at      time.Time
}

func (r *Relay) hold(p *part) {
	size := proto.Size(p.msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.records > maxHeld || size > maxHeldBytes {
		r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
		return
	}
	// Evict the oldest rather than refuse the newcomer, the likeliest to be claimed soon.
	for r.heldN+p.records > maxHeld || r.heldBytes+size > maxHeldBytes {
		if !r.evictOldestLocked() {
			r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
			return
		}
	}
	r.held[p.session] = append(r.held[p.session], heldPart{p, r.opts.Now(), size})
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
			c, pol, why, ok, how := r.decide(key, h.p.pid, h.p.at, h.p.narrow)
			limit := hold
			if h.p.start && why == whyUnclaimed {
				// A conversation start waits for the thread's first turn.
				limit = max(hold, r.opts.TraceHold)
			}
			if !ok && !h.p.narrow && why != whyNoKey && now.Sub(h.at) >= limit {
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

func (r *Relay) learnTrace(traceID, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.traces[traceID]; !known && len(r.traces) >= maxTraces {
		r.stats.add("trace_index_full", 1)
		return
	}
	r.traces[traceID] = traceSession{session, r.opts.Now()}
}

func (r *Relay) traceOf(traceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.traces[traceID].session
}

// sessionFor resolves a held key to a session ("" for a trace not yet named).
func (r *Relay) sessionFor(key string) string {
	if id, ok := strings.CutPrefix(key, tracePrefix); ok {
		return r.traceOf(id)
	}
	return key
}
