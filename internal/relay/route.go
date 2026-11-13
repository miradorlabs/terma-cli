package relay

import (
	"slices"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Why a part cannot leave now; the same reason names the drop when its hold runs out.
const (
	whyNoTrace        = "no_session_trace"   // a span of a trace no record has named yet
	whyUnclaimed      = "unclaimed_expired"  // no hook in a collected repository claimed the session
	whyProcess        = "uncovered_process"  // claimed, but by other processes: resumed elsewhere
	whyNoKey          = "no_key"             // claimed, but this machine holds no key for the project
	whyAmbiguous      = "ambiguous_process"  // no session named; its process named more than one session
	whyProcessIdle    = "no_session_process" // no session named; its process has named none
	whyProcessRunning = "process_running"    // no session named; attributed only once its process exits
	whyWidened        = "policy_widened"     // arrived outside global mode, now placed only by it: dropped at once
	whyNotCollected   = "not_collected"      // a hook marked the session where its team does not collect: dropped at once
)

// attribution says how a part's session was found, when not by the part itself.
type attribution struct {
	how     string
	session string
}

// decide reports whether a part may leave now, with which claim and policy, or why not.
// A part is placed only by what it or its trace names; a part naming no session waits
// for its process to exit (decideExited), because a shared process that has shown one
// claimed session may be about to name a personal one: loss, never a guess. narrow and
// claimed are the part's (part.narrow, part.claimed): global mode's catch-all may take neither.
func (r *Relay) decide(key string, pid int, at time.Time, narrow, claimed bool) (claim.Claim, Policy, string, bool, attribution) {
	if c, global := r.catchAll(); global && !narrow && !claimed {
		pol, ok := r.resolve(c)
		if !ok {
			return claim.Claim{}, Policy{}, whyNoKey, false, attribution{}
		}
		if !pol.RequireClaim {
			return c, pol, "", true, attribution{how: semconv.TermaRelayAttributionCatchAll}
		}
	}
	if p, ok := strings.CutPrefix(key, procPrefix); ok {
		n, _ := strconv.Atoi(p)
		if r.collectsNone(n, at) {
			return claim.Claim{}, Policy{}, whyNotCollected, false, attribution{}
		}
		return r.decideExited(n, at, narrow)
	}
	session := r.sessionFor(key)
	if session == "" {
		if pid != 0 {
			c, pol, why, ok, how := r.decideExited(pid, at, narrow)
			if ok {
				return c, pol, "", true, how
			}
			if why == whyNotCollected {
				return claim.Claim{}, Policy{}, why, false, attribution{}
			}
		}
		return claim.Claim{}, Policy{}, whyNoTrace, false, attribution{}
	}
	c, pol, why, ok := r.decideClaimed(session, pid, at, narrow)
	return c, pol, why, ok, attribution{}
}

// decideClaimed places a part by its session's claim: the placement covering pid at the part's time.
// A narrow part never leaves under a policy that needs no claim: only global mode grants
// one, so the part's session was not collected when it arrived, whatever repository it is in.
func (r *Relay) decideClaimed(session string, pid int, at time.Time, narrow bool) (claim.Claim, Policy, string, bool) {
	c, ok := r.lookup(session)
	if !ok {
		return claim.Claim{}, Policy{}, whyUnclaimed, false
	}
	c, ok = c.At(pid, at)
	if !ok {
		return claim.Claim{}, Policy{}, whyProcess, false
	}
	// The placement's own project: a mark has none, and never reaches a key or a route.
	if c.ProjectID == "" {
		return claim.Claim{}, Policy{}, whyNotCollected, false
	}
	pol, ok := r.resolve(c)
	if !ok {
		return claim.Claim{}, Policy{}, whyNoKey, false
	}
	if narrow && !pol.RequireClaim {
		return claim.Claim{}, Policy{}, whyWidened, false
	}
	return c, pol, "", true
}

// verdict is what deciding a part found: whether it leaves now, with which claim and
// policy and, on a migrated machine, for which enrollment; else why not.
type verdict struct {
	c   claim.Claim
	pol Policy
	why string
	ok  bool
	how attribution
	// q is the destination on a migrated machine (decideTeams); nil under the single-team
	// rules (decide).
	q *Queued
	// final drops the part now instead of holding it; decide's own such reasons
	// (whyNotCollected, whyWidened) are told by name.
	final bool
	// blocker, mixedAt and session are the router's Routed.Blocker and Routed.MixedAt, and
	// the session it was told of.
	blocker string
	mixedAt time.Time
	session string
}

// judge decides a part under the rules in force: by teams on a migrated machine, else as
// decide does.
func (r *Relay) judge(key string, p *part) verdict {
	if r.teams() {
		return r.decideTeams(key, p)
	}
	c, pol, why, ok, how := r.decide(key, p.pid, p.at, p.narrow, p.claimed)
	return verdict{c: c, pol: pol, why: why, ok: ok, how: how}
}

// route sends a part on only if nothing is held for its key, so arrival order holds, and
// drops a part of a session marked not collected without holding it. On a migrated machine
// a part whose records fall under different placements is routed piece by piece.
func (r *Relay) route(p *part) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	for _, piece := range r.pieces(p.session, p) {
		r.routePiece(piece)
	}
}

func (r *Relay) routePiece(p *part) {
	v := r.judge(p.session, p)
	r.mixed(v)
	if v.why == whyNotCollected || v.final {
		r.stats.dropped(p.signal, v.why, p.records)
		r.block(v.blocker, p.records)
		return
	}
	if v.ok {
		r.mu.Lock()
		waiting := len(r.held[p.session]) > 0
		r.mu.Unlock()
		if !waiting {
			r.deliver(v, p)
			return
		}
	}
	r.hold(p)
}

// deliver sends on a part judge let leave, under the rules it was judged by. deliverMu is held.
func (r *Relay) deliver(v verdict, p *part) {
	if v.q != nil {
		r.deliverTeam(v, p)
		return
	}
	r.deliverAttributed(v.c, v.pol, p, v.how)
}

// deliverAttributed applies the content policy, stamps the project and enqueues. deliverMu is held.
func (r *Relay) deliverAttributed(c claim.Claim, pol Policy, p *part, how attribution) {
	if pol.Unadmitted {
		r.stats.dropped(p.signal, "policy_repository", p.records)
		return
	}
	if !r.dress(pol, p, c.ProjectID, r.ranIn(c, p, how), how) {
		return
	}
	r.enqueue(c, p)
}

// dress applies pol to p and stamps it for project, naming the checkout ran: what it may
// carry, the project, the relay's version, the checkout and user, and how the relay placed
// it. False when pol sends none of p's signal, which is dropped.
func (r *Relay) dress(pol Policy, p *part, project string, ran claim.Claim, how attribution) bool {
	if pol.Signals != nil && !contains(pol.Signals, string(p.signal)) {
		r.stats.dropped(p.signal, "policy_signal", p.records)
		return false
	}
	unclassified := map[string]int{}
	if n := r.rules.withhold(p, pol.IncludePrompts, pol.IncludeToolContent, unclassified); n > 0 {
		r.stats.add("withheld_content_records", n)
	}
	for key, n := range unclassified {
		r.stats.unclassified(key, n)
	}
	// terma.relay.attribution and terma.relay.session.id mark a part the relay placed by
	// inference, so the backend can tell the relay's join from its own.
	stamp(p, semconv.MiradorProjectIDKey, project)
	// terma.version is the relay that accepted the part: the outbox keeps it across an upgrade.
	if r.opts.Version != "" {
		stamp(p, semconv.TermaVersionKey, r.opts.Version)
	}
	stampCheckout(p, ran, pol.IncludeToolContent)
	nameUser(p, pol)
	if how.how != "" {
		stamp(p, semconv.TermaRelayAttributionKey, how.how)
		if how.session != "" {
			stamp(p, semconv.TermaRelaySessionIDKey, how.session)
		}
		// attributed_by_process or attributed_by_catch_all: global mode's default project is no process's.
		r.stats.add("attributed_by_"+strings.ReplaceAll(how.how, "-", "_")+"."+string(p.signal), p.records)
	}
	return true
}

// ranIn is the claim naming where the part's session ran, its Root the working tree and its
// Cwd the directory: the placing claim, or, where global mode's catch-all placed the part
// without either, its session's own claim for the same project. Its checkout is empty when no
// hook has named one.
func (r *Relay) ranIn(c claim.Claim, p *part, how attribution) claim.Claim {
	if c.Root != "" || c.Cwd != "" || how.how != semconv.TermaRelayAttributionCatchAll {
		return c
	}
	session := r.sessionFor(p.session)
	if session == "" || strings.HasPrefix(session, procPrefix) {
		return claim.Claim{}
	}
	own, ok := r.lookup(session)
	if !ok {
		return claim.Claim{}
	}
	if own, ok = own.At(p.pid, p.at); !ok || own.ProjectID != c.ProjectID {
		return claim.Claim{}
	}
	return own
}

// catchAll is global mode's claim for what nothing placed; a migrated machine has none,
// its Unknown rule being the router's.
func (r *Relay) catchAll() (claim.Claim, bool) {
	if r.opts.CatchAll == nil || r.teams() {
		return claim.Claim{}, false
	}
	return r.opts.CatchAll()
}

// stamp sets a resource attribute on every resource, replacing any value: the relay decides, not the agent.
func stamp(p *part, key, value string) { stampValue(p, key, strValue(value)) }

func stampValue(p *part, key string, value *commonpb.AnyValue) {
	set := func(res *resourcepb.Resource) {
		for _, kv := range res.Attributes {
			if kv.GetKey() == key {
				kv.Value = proto.CloneOf(value)
				return
			}
		}
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: key, Value: proto.CloneOf(value)})
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

// unstamp removes a resource attribute from every resource.
func unstamp(p *part, key string) {
	drop := func(res *resourcepb.Resource) {
		if res != nil {
			res.Attributes = slices.DeleteFunc(res.Attributes, func(kv *commonpb.KeyValue) bool { return kv.GetKey() == key })
		}
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			drop(rl.Resource)
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			drop(rs.Resource)
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			drop(rm.Resource)
		}
	}
}

func strValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
