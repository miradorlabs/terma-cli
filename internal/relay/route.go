package relay

import (
	"context"
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

// Why a part cannot leave now; the same reason names the drop when its hold runs out.
const (
	whyNoTrace        = "no_session_trace"   // a span of a trace no record has named yet
	whyUnclaimed      = "unclaimed_expired"  // no hook of an opted-in repository claimed the session
	whyProcess        = "uncovered_process"  // claimed, but by other processes: resumed elsewhere
	whyNoKey          = "no_key"             // claimed, but this machine holds no key for the project
	whyAmbiguous      = "ambiguous_process"  // no session named; its process named more than one session
	whyProcessIdle    = "no_session_process" // no session named; its process has named none
	whyProcessRunning = "process_running"    // no session named; attributed only once its process exits
)

// procPrefix keys a part that names neither session nor trace by the process that sent it.
const procPrefix = "proc:"

func procKey(pid int) string { return procPrefix + strconv.Itoa(pid) }

// Past maxProcSessions a process counts as serving many sessions.
const (
	maxProcs        = 4096
	maxProcSessions = 256
)

// exitGrace lets an exporter's shutdown flush still name a session before its process is attributed.
const exitGrace = 10 * time.Second

type procState struct {
	sessions map[string]bool
	overflow bool
	lastSeen time.Time
	exitedAt time.Time
}

// attribution says how a part's session was found, when not by the part itself.
type attribution struct {
	how     string
	session string
}

// decide reports whether a part may leave now, with which claim and policy, or why not.
// A part is placed only by what it or its trace names; a part naming no session waits
// for its process to exit (decideExited), because a shared process that has shown one
// claimed session may be about to name a personal one: loss, never a guess.
func (r *Relay) decide(key string, pid int, at time.Time) (claim.Claim, Policy, string, bool, attribution) {
	if c, global := r.catchAll(); global {
		pol, ok := r.resolve(c)
		if !ok {
			return claim.Claim{}, Policy{}, whyNoKey, false, attribution{}
		}
		if !pol.RequireClaim {
			return c, pol, "", true, attribution{how: "catch-all"}
		}
	}
	if p, ok := strings.CutPrefix(key, procPrefix); ok {
		n, _ := strconv.Atoi(p)
		return r.decideExited(n)
	}
	session := r.sessionFor(key)
	if session == "" {
		if pid != 0 {
			if c, pol, _, ok, how := r.decideExited(pid); ok {
				return c, pol, "", true, how
			}
		}
		return claim.Claim{}, Policy{}, whyNoTrace, false, attribution{}
	}
	c, pol, why, ok := r.decideClaimed(session, pid, at)
	return c, pol, why, ok, attribution{}
}

// decideExited attributes a sessionless part to the one session its exited sender named, if claimed.
func (r *Relay) decideExited(pid int) (claim.Claim, Policy, string, bool, attribution) {
	r.mu.Lock()
	st := r.procs[pid]
	var sessions []string
	var exited time.Time
	overflow := false
	if st != nil {
		for s := range st.sessions {
			sessions = append(sessions, s)
		}
		exited, overflow = st.exitedAt, st.overflow
	}
	r.mu.Unlock()
	switch {
	case st == nil || len(sessions) == 0:
		return claim.Claim{}, Policy{}, whyProcessIdle, false, attribution{}
	case exited.IsZero() || r.opts.Now().Sub(exited) < exitGrace:
		return claim.Claim{}, Policy{}, whyProcessRunning, false, attribution{}
	case overflow || len(sessions) != 1:
		return claim.Claim{}, Policy{}, whyAmbiguous, false, attribution{}
	}
	c, pol, why, ok := r.decideClaimed(sessions[0], pid, time.Time{})
	if !ok {
		return claim.Claim{}, Policy{}, why, false, attribution{}
	}
	return c, pol, "", true, attribution{how: "process", session: sessions[0]}
}

func (r *Relay) learnProcess(pid int, session string) {
	if pid == 0 || session == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.procs[pid]
	if st == nil {
		if len(r.procs) >= maxProcs {
			r.stats.add("process_index_full", 1)
			return
		}
		st = &procState{sessions: map[string]bool{}}
		r.procs[pid] = st
	}
	st.lastSeen = r.opts.Now()
	if !st.sessions[session] {
		if len(st.sessions) >= maxProcSessions {
			st.overflow = true
			return
		}
		st.sessions[session] = true
	}
}

func (r *Relay) watchProcesses(now time.Time) {
	alive := r.opts.ProcessAlive
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, st := range r.procs {
		switch {
		case !st.exitedAt.IsZero():
			// Kept past the longest hold, so the sweep that drops its parts still knows why.
			if now.Sub(st.exitedAt) > 2*r.opts.TraceHold {
				delete(r.procs, pid)
			}
		case alive != nil && !alive(pid):
			st.exitedAt = now
		case now.Sub(st.lastSeen) > 2*r.opts.TraceHold:
			// Never exits, or its pid was reused; either way nothing held can be its.
			delete(r.procs, pid)
		}
	}
}

// decideClaimed places a part by its session's claim: the placement covering pid at the part's time.
func (r *Relay) decideClaimed(session string, pid int, at time.Time) (claim.Claim, Policy, string, bool) {
	c, ok := r.lookup(session)
	if !ok {
		return claim.Claim{}, Policy{}, whyUnclaimed, false
	}
	c, ok = c.At(pid, at)
	if !ok {
		return claim.Claim{}, Policy{}, whyProcess, false
	}
	pol, ok := r.resolve(c)
	if !ok {
		return claim.Claim{}, Policy{}, whyNoKey, false
	}
	return c, pol, "", true
}

// route sends a part on only if nothing is held for its key, so arrival order holds.
func (r *Relay) route(p *part) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	if c, pol, _, ok, how := r.decide(p.session, p.pid, p.at); ok {
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

// AttributionAttr and InferredSessionAttr mark a part the relay placed by inference, so the
// backend can tell the relay's join from its own.
const (
	AttributionAttr     = "terma.relay.attribution"
	InferredSessionAttr = "terma.relay.session.id"
)

// deliverAttributed applies the content policy, stamps the project and enqueues. deliverMu is held.
func (r *Relay) deliverAttributed(c claim.Claim, pol Policy, p *part, how attribution) {
	if len(pol.ExcludePaths) > 0 {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
	}
	if pathExcluded(p.msg, pol.ExcludePaths) {
		r.stats.dropped(p.signal, "policy_path", p.records)
		return
	}
	if pol.Signals != nil && !contains(pol.Signals, string(p.signal)) {
		r.stats.dropped(p.signal, "policy_signal", p.records)
		return
	}
	unclassified := map[string]int{}
	if n := r.rules.withhold(p, pol.IncludePrompts, pol.IncludeToolContent, unclassified); n > 0 {
		r.stats.add("withheld_content_records", n)
	}
	for key, n := range unclassified {
		r.stats.unclassified(key, n)
	}
	stamp(p, ProjectAttr, c.ProjectID)
	if how.how != "" {
		stamp(p, AttributionAttr, how.how)
		if how.session != "" {
			stamp(p, InferredSessionAttr, how.session)
		}
		r.stats.add("attributed_by_process."+string(p.signal), p.records)
	}
	r.noteDelivery(p)
	r.enqueue(c, p)
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
			c, pol, why, ok, how := r.decide(key, h.p.pid, h.p.at)
			limit := hold
			if h.p.start && why == whyUnclaimed {
				// A conversation start waits for the thread's first turn.
				limit = max(hold, r.opts.TraceHold)
			}
			if !ok && why != whyNoKey && now.Sub(h.at) >= limit {
				if cc, cok := r.catchAll(); cok {
					if cpol, pok := r.resolve(cc); pok && !cpol.RequireClaim {
						c, pol, ok, how = cc, cpol, true, attribution{how: "catch-all"}
						r.stats.add("caught_by_default."+string(h.p.signal), h.p.records)
					}
				}
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

func (r *Relay) catchAll() (claim.Claim, bool) {
	if r.opts.CatchAll == nil {
		return claim.Claim{}, false
	}
	return r.opts.CatchAll()
}

// Run releases, drops and delivers held records until ctx is done, then stops every destination.
func (r *Relay) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// The janitor weighs files by modification time, so it keeps the wall clock, never Options.Now.
	r.janitor(time.Now())
	r.recoverOutbox()
	lastJanitor := time.Now()
	// The first beat comes a minute after start, so a restarted relay says so soon.
	lastBeat := r.opts.Now().Add(min(time.Minute, r.opts.HeartbeatEvery) - r.opts.HeartbeatEvery)
	beaten := false
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
			r.countQueuedAtExit()
			return
		case <-tick.C:
			r.sweep()
			if now := time.Now(); now.Sub(lastJanitor) >= time.Minute {
				r.janitor(now)
				lastJanitor = now
			}
			if now := r.opts.Now(); now.Sub(lastBeat) >= r.opts.HeartbeatEvery {
				// Off the sweep's goroutine, so a slow host cannot hold up the relay.
				reason := HeartbeatInterval
				if !beaten {
					reason, beaten = HeartbeatStart, true
				}
				go func() { _ = r.heartbeat(ctx, reason) }()
				lastBeat = now
			}
		}
	}
}

func (r *Relay) janitor(now time.Time) {
	for rt, n := range r.sweepOutbox(now) {
		r.mu.Lock()
		s := r.senders[rt]
		r.mu.Unlock()
		if s != nil {
			s.delivered(n)
		}
	}
}

// countQueuedAtExit counts the outbox as queued, not lost: the next relay delivers it.
func (r *Relay) countQueuedAtExit() {
	routes, _ := r.outbox.routes()
	for _, rt := range routes {
		entries, _ := r.outbox.list(rt)
		for _, e := range entries {
			r.stats.add("queued_at_exit."+string(e.signal), e.records)
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
	for _, s := range r.senders {
		if s.busy() {
			return 0, false
		}
	}
	return r.opts.Now().Sub(r.lastSeen), true
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

// lookupCache makes a busy session cost one file read a second rather than one per part,
// with deliverMu held.
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

// stamp sets a resource attribute on every resource, replacing any value: the relay decides, not the agent.
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
