package relay

import (
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Why a part cannot leave now; the same reason names the drop when its hold runs out.
const (
	whyNoTrace        = "no_session_trace"   // a span of a trace no record has named yet
	whyUnclaimed      = "unclaimed_expired"  // no hook in a collected folder claimed the session
	whyProcess        = "uncovered_process"  // claimed, but by other processes: resumed elsewhere
	whyNoKey          = "no_key"             // claimed, but this machine holds no key for the project
	whyAmbiguous      = "ambiguous_process"  // no session named; its process named more than one session
	whyProcessIdle    = "no_session_process" // no session named; its process has named none
	whyProcessRunning = "process_running"    // no session named; attributed only once its process exits
)

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

// AttributionAttr and InferredSessionAttr mark a part the relay placed by inference, so the
// backend can tell the relay's join from its own.
const (
	AttributionAttr     = "terma.relay.attribution"
	InferredSessionAttr = "terma.relay.session.id"
)

// deliverAttributed applies the content policy, stamps the project and enqueues. deliverMu is held.
func (r *Relay) deliverAttributed(c claim.Claim, pol Policy, p *part, how attribution) {
	if pol.Unadmitted {
		r.stats.dropped(p.signal, "policy_repository", p.records)
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
	r.enqueue(c, p)
}

func (r *Relay) catchAll() (claim.Claim, bool) {
	if r.opts.CatchAll == nil {
		return claim.Claim{}, false
	}
	return r.opts.CatchAll()
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
