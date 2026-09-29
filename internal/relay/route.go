package relay

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Routing. Every part is keyed by its session (or, for a span naming none, by its
// trace). decide says whether it may leave now; if not, it waits in the hold until
// the reason goes away — a claim lands, the trace is named, a key appears — or the hold
// runs out, and then it is dropped for that reason. A session's parts leave in the
// order they arrived: while any are held, a newcomer queues behind them.

// traceTTL is how long the relay remembers which session a trace belongs to.
const traceTTL = time.Hour

// maxHeld and maxHeldBytes bound what waits at once, by records and by encoded size.
// When full, the oldest parts are evicted (unclaimed_evicted), unnamed traces first.
const (
	maxHeld      = 50000
	maxHeldBytes = 64 << 20
)

// maxTraces bounds the trace → session index; past it no new trace is learnt
// (trace_index_full) until old ones age out.
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

// Why a part cannot leave now. The same reason names the drop when its hold runs out.
const (
	whyNoTrace     = "no_session_trace"   // a span of a trace no record has named yet
	whyUnclaimed   = "unclaimed_expired"  // no hook of an opted-in repository claimed the session
	whyProcess     = "uncovered_process"  // claimed, but by other processes: resumed elsewhere
	whyNoKey       = "no_key"             // claimed, but this machine holds no key for the project
	whyAmbiguous   = "ambiguous_process"  // no session named; its process exports for several projects or sessions not all opted in
	whyProcessIdle = "no_session_process" // no session named; its process has named none yet
)

// procPrefix keys a part that names no session and no trace — Codex's metrics — by
// the process that sent it.
const procPrefix = "proc:"

func procKey(pid int) string { return procPrefix + strconv.Itoa(pid) }

// procWindow is how long a session a process exported counts as that process's for
// attribution by process: an hour, like the trace index.
const procWindow = time.Hour

// maxProcs bounds the process → sessions index.
const maxProcs = 4096

// attribution says how a part's session was found, when not by the part itself.
type attribution struct {
	how     string // "process"
	session string // the one session the process exported, if it was one
}

// decide reports whether a part under key, sent by pid, may leave now, and if so
// with which claim and policy, and how its session was found when not by the part
// itself; if not, why not.
func (r *Relay) decide(key string, pid int) (claim.Claim, Policy, string, bool, attribution) {
	if p, ok := strings.CutPrefix(key, procPrefix); ok {
		n, _ := strconv.Atoi(p)
		return r.decideProcess(n)
	}
	session := r.sessionFor(key)
	if session == "" {
		// A span of a trace nothing has named — mostly process-level work. Its process
		// may still tell where it belongs.
		if pid != 0 {
			if c, pol, _, ok, how := r.decideProcess(pid); ok {
				return c, pol, "", true, how
			}
		}
		return claim.Claim{}, Policy{}, whyNoTrace, false, attribution{}
	}
	return r.decideSession(session, pid)
}

// isInternal reports whether session's start said Codex made it for itself: the
// title generator runs with approval "never" in a read-only sandbox, where a thread a
// developer started, or resumed from elsewhere, carries the developer's own policies.
// A session whose start was never seen is not internal: nothing is adopted on a guess.
func (r *Relay) isInternal(session string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.internal[session]
	return ok
}

// singleWorkspace are the Codex clients whose process serves one repository: every
// real thread it runs is in that repository, so its hooks claim it, and a conversation
// no hook claimed is Codex's own work for those threads — the title generator the TUI
// starts after the first prompt. Multi-workspace clients (Desktop, the IDE extension's
// app server) can hold a personal thread beside a claimed one; nothing is adopted there.
var singleWorkspace = map[string]bool{"codex-tui": true, "codex_exec": true}

// adoptsLocked reports whether pid is a single-workspace client: every client it has
// served named itself one. Codex's shared app-server daemon runs threads for the TUI
// and for Desktop in one process, so one TUI thread does not make it single-workspace.
// r.mu must be held.
func (r *Relay) adoptsLocked(pid int) bool {
	o := r.origins[pid]
	if len(o) == 0 {
		return false
	}
	for name := range o {
		if !singleWorkspace[name] {
			return false
		}
	}
	return true
}

// processProject is where a process's work belongs: the project that every session
// the process exported lately and that a hook claimed maps to, under one policy. A
// single-workspace client's unclaimed sessions (see singleWorkspace) go along with it;
// anyone else's make the process ambiguous. root is the one claimed session, if there
// is exactly one. exclude leaves one session out (the one being adopted).
func (r *Relay) processProject(pid int, exclude string) (c claim.Claim, pol Policy, root, why string, ok bool) {
	r.mu.Lock()
	var sessions []string
	for s, at := range r.procs[pid] {
		if s != exclude && r.opts.Now().Sub(at) < procWindow {
			sessions = append(sessions, s)
		}
	}
	adopts := r.adoptsLocked(pid)
	r.mu.Unlock()
	sort.Strings(sessions)
	var claimed []string
	for _, s := range sessions {
		sc, spol, swhy, sok := r.decideClaimed(s, pid)
		switch {
		case sok && len(claimed) == 0:
			c, pol = sc, spol
			claimed = append(claimed, s)
		case sok:
			if sc.ProjectID != c.ProjectID || spol != pol {
				return claim.Claim{}, Policy{}, "", whyAmbiguous, false
			}
			claimed = append(claimed, s)
		case swhy == whyUnclaimed && adopts && r.isInternal(s):
			// Codex's own work in this repository: it goes with the claimed threads.
		case len(sessions) == 1:
			return claim.Claim{}, Policy{}, "", swhy, false
		default:
			return claim.Claim{}, Policy{}, "", whyAmbiguous, false
		}
	}
	if len(claimed) == 0 {
		if len(sessions) == 0 {
			return claim.Claim{}, Policy{}, "", whyProcessIdle, false
		}
		return claim.Claim{}, Policy{}, "", whyUnclaimed, false
	}
	if len(claimed) == 1 {
		root = claimed[0]
	}
	return c, pol, root, "", true
}

// decideProcess attributes a part that names no session — Codex's metrics, and spans
// of a trace nothing named — by its sender: it may leave when the sender's work belongs
// to one project (processProject). A counter cannot be split, so a process working for
// several projects, or for sessions not all opted in, has its part dropped, not
// guessed. Codex exec and the TUI serve one repository; their metrics find it this way.
func (r *Relay) decideProcess(pid int) (claim.Claim, Policy, string, bool, attribution) {
	c, pol, root, why, ok := r.processProject(pid, "")
	if !ok {
		return claim.Claim{}, Policy{}, why, false, attribution{}
	}
	return c, pol, "", true, attribution{how: "process", session: root}
}

// learnProcess records that pid exported for session, from a client that named
// itself originator (Codex's attribute; "" for the others).
func (r *Relay) learnProcess(pid int, session, originator string) {
	if pid == 0 || session == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.procs[pid]
	if m == nil {
		if len(r.procs) >= maxProcs {
			r.stats.add("process_index_full", 1)
			return
		}
		m = map[string]time.Time{}
		r.procs[pid] = m
	}
	m[session] = r.opts.Now()
	if originator != "" {
		o := r.origins[pid]
		if o == nil {
			o = map[string]bool{}
			r.origins[pid] = o
		}
		o[originator] = true
	}
}

// decideSession is decide for a part that names its session. A session no hook
// claimed is adopted into its process's project when the process is a single-workspace
// client whose claimed sessions all belong to one project: Codex's title conversation.
func (r *Relay) decideSession(session string, pid int) (claim.Claim, Policy, string, bool, attribution) {
	c, pol, why, ok := r.decideClaimed(session, pid)
	if ok || why != whyUnclaimed || pid == 0 {
		return c, pol, why, ok, attribution{}
	}
	r.mu.Lock()
	adopts := r.adoptsLocked(pid)
	r.mu.Unlock()
	if !adopts || !r.isInternal(session) {
		return c, pol, why, false, attribution{}
	}
	pc, ppol, root, _, pok := r.processProject(pid, session)
	if !pok {
		return claim.Claim{}, Policy{}, whyUnclaimed, false, attribution{}
	}
	return pc, ppol, "", true, attribution{how: "process-sibling", session: root}
}

// decideClaimed is decideSession by the session's own claim alone.
func (r *Relay) decideClaimed(session string, pid int) (claim.Claim, Policy, string, bool) {
	c, ok := r.lookup(session)
	if !ok {
		return claim.Claim{}, Policy{}, whyUnclaimed, false
	}
	if !c.Covers(pid) {
		return claim.Claim{}, Policy{}, whyProcess, false
	}
	pol, ok := r.resolve(c)
	if !ok {
		return claim.Claim{}, Policy{}, whyNoKey, false
	}
	return c, pol, "", true
}

// route sends a part on if decide allows it and its key has nothing held, and holds
// it otherwise.
func (r *Relay) route(p *part) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	if c, pol, _, ok, how := r.decide(p.session, p.pid); ok {
		r.mu.Lock()
		waiting := len(r.held[p.session]) > 0
		r.mu.Unlock()
		if !waiting {
			r.deliverAttributed(c, pol, p, how)
			return
		}
	}
	r.hold(p)
}

func (r *Relay) hold(p *part) {
	size := proto.Size(p.msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.records > maxHeld || size > maxHeldBytes {
		r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
		return
	}
	// Full: evict the oldest rather than refuse the newcomer, the likeliest to be
	// claimed soon.
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

// evictOldestLocked drops the oldest held part — of an unnamed trace if there is one,
// those being mostly process-level work that never will be named — and reports
// whether it found any. r.mu is held.
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

// AttributionAttr and InferredSessionAttr mark, on the resource, a part the relay
// attributed by its process rather than by a session it named: "process", and the one
// session that process exported when there was exactly one. Inferred, and said so, so
// the backend can tell a join it made from one the relay made.
const (
	AttributionAttr     = "terma.relay.attribution"
	InferredSessionAttr = "terma.relay.session.id"
)

// deliverAttributed applies the project's content policy, stamps the project (and,
// for an inferred session, how it was found) on the part and hands it to the
// project's destination. deliverMu is held.
func (r *Relay) deliverAttributed(c claim.Claim, pol Policy, p *part, how attribution) {
	if n := withhold(p, pol.IncludePrompts, pol.IncludeToolContent); n > 0 {
		r.stats.add("withheld_content_records", n)
	}
	stamp(p, ProjectAttr, c.ProjectID)
	if how.how != "" {
		stamp(p, AttributionAttr, how.how)
		if how.session != "" {
			stamp(p, InferredSessionAttr, how.session)
		}
		r.stats.add("attributed_by_process."+string(p.signal), p.records)
	}
	r.destination(pol).enqueue(p)
}

// sweep releases held parts whose reason went away, in arrival order, drops those whose
// hold ran out, and ages out the trace index and the caches.
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
	for pid, sessions := range r.procs {
		for s, at := range sessions {
			if now.Sub(at) >= procWindow {
				delete(sessions, s)
			}
		}
		if len(sessions) == 0 {
			delete(r.procs, pid)
			delete(r.origins, pid)
		}
	}
	for s, at := range r.internal {
		if now.Sub(at) >= procWindow {
			delete(r.internal, s)
		}
	}
	r.mu.Unlock()
	r.cache.expire(now)

	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	for _, key := range keys {
		hold := r.opts.Hold
		if strings.HasPrefix(key, tracePrefix) {
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
		// Every part that may leave does, in arrival order. A part that must wait (one
		// from a process the claim does not cover) holds up nothing: order matters only
		// among parts that leave, and those belong to other runs.
		for _, h := range parts {
			c, pol, why, ok, how := r.decide(key, h.p.pid)
			limit := hold
			if h.p.start && why == whyUnclaimed {
				// A conversation start waits for the thread's first turn (part.start).
				limit = max(hold, r.opts.TraceHold)
			}
			switch {
			case ok:
				out = append(out, release{c, pol, h.p, how})
			case now.Sub(h.at) >= limit:
				r.stats.dropped(h.p.signal, why, h.p.records)
				if r.opts.Logf != nil {
					c, _ := r.lookup(r.sessionFor(key))
					r.opts.Logf("drop %s %s: %d %s from pid %d; claim %q pids %v", why, key, h.p.records, h.p.signal, h.p.pid, c.ProjectID, c.PIDs)
				}
			default:
				keep = append(keep, h)
			}
		}
		// Nothing else holds a part while this runs: hold is only called under
		// deliverMu, which the sweep has.
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

// Run releases held records as their reasons go away, drops those that outlive the
// hold, and delivers until ctx is done. On return every destination has stopped; what
// the grace could not send is counted as lost.
func (r *Relay) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Claims that landed since the last sweep still count.
			r.sweep()
			r.deliverMu.Lock()
			r.mu.Lock()
			for key, parts := range r.held {
				for _, h := range parts {
					reason := "unclaimed_at_exit"
					if strings.HasPrefix(key, tracePrefix) {
						reason = "no_session_trace_at_exit"
					}
					r.stats.dropped(h.p.signal, reason, h.p.records)
				}
				delete(r.held, key)
			}
			r.heldN, r.heldBytes = 0, 0
			r.mu.Unlock()
			r.deliverMu.Unlock()
			close(r.stopping)
			done := make(chan struct{})
			go func() { r.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(r.opts.Grace):
				r.cancelSend()
				<-done
			}
			r.cancelSend()
			return
		case <-tick.C:
			r.sweep()
		}
	}
}

// Idle reports how long the relay has had no export, if it holds and queues nothing.
func (r *Relay) Idle() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heldN > 0 {
		return 0, false
	}
	for _, d := range r.dests {
		if d.busy() {
			return 0, false
		}
	}
	return r.opts.Now().Sub(r.lastSeen), true
}

// --- traces ------------------------------------------------------------------------

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

// sessionFor resolves a held key to a session: itself, or for a trace key, the
// session its trace has since been seen to belong to ("" while unknown).
func (r *Relay) sessionFor(key string) string {
	if id, ok := strings.CutPrefix(key, tracePrefix); ok {
		return r.traceOf(id)
	}
	return key
}

// --- claims and policies, cached -----------------------------------------------------

// lookupCache keeps claim and policy lookups for a short while (Options.ClaimCacheTTL,
// PolicyCacheTTL), so a busy session costs one file read a second rather than one per
// part, with deliverMu held. Unclaimed sessions are cached too.
type lookupCache struct {
	mu       sync.Mutex
	claims   map[string]cachedClaim
	policies map[string]cachedPolicy
}

type cachedClaim struct {
	c  claim.Claim
	ok bool
	at time.Time
}

type cachedPolicy struct {
	pol Policy
	ok  bool
	at  time.Time
}

func (r *Relay) lookup(session string) (claim.Claim, bool) {
	now := r.opts.Now()
	if ttl := r.opts.ClaimCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.claims[session]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			return e.c, e.ok
		}
	}
	c, ok := r.opts.Lookup(session, now)
	if r.opts.ClaimCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.claims[session] = cachedClaim{c, ok, now}
		r.cache.mu.Unlock()
	}
	return c, ok
}

func (r *Relay) resolve(c claim.Claim) (Policy, bool) {
	now := r.opts.Now()
	key := c.ProjectID + "\x00" + c.Tool
	if ttl := r.opts.PolicyCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			return e.pol, e.ok
		}
	}
	pol, err := r.opts.Resolve(c)
	ok := err == nil && pol.Endpoint != "" && pol.Key != ""
	if r.opts.PolicyCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.policies[key] = cachedPolicy{pol, ok, now}
		r.cache.mu.Unlock()
	}
	return pol, ok
}

func (lc *lookupCache) expire(now time.Time) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	for k, e := range lc.claims {
		if now.Sub(e.at) > time.Minute {
			delete(lc.claims, k)
		}
	}
	for k, e := range lc.policies {
		if now.Sub(e.at) > time.Minute {
			delete(lc.policies, k)
		}
	}
}

// --- stamping ----------------------------------------------------------------------

// stamp sets a resource attribute on every resource in the part, replacing any value
// already there: the relay decides, not the agent.
func stamp(p *part, key, value string) {
	set := func(res *resourcepb.Resource) {
		for _, kv := range res.Attributes {
			if kv.GetKey() == key {
				kv.Value = strValue(value)
				return
			}
		}
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: key, Value: strValue(value)})
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			set(rl.Resource)
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			set(rs.Resource)
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			set(rm.Resource)
		}
	}
}

func strValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
