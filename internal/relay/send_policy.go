package relay

import (
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// sendTally is what applying the policy to a queued body took from it.
type sendTally struct {
	excluded, withheld int
	unclassified       map[string]int
}

// count records what the policy took from a body that was sent.
func (r *Relay) count(sig Signal, tl sendTally) {
	if tl.excluded > 0 {
		r.stats.dropped(sig, "policy_path", tl.excluded)
	}
	if tl.withheld > 0 {
		r.stats.add("withheld_at_send_records", tl.withheld)
	}
	for key, c := range tl.unclassified {
		r.stats.unclassified(key, c)
	}
}

// withholdQueued applies the policy as it stands now to a queued body, reporting what it
// took; nil drops all of it (an excluded path took everything when it reports any), as
// does a body that no longer decodes, since it cannot be checked against a stricter policy.
func (r *Relay) withholdQueued(sig Signal, body []byte, pol Policy) ([]byte, sendTally) {
	if pol.Excludes != nil {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
	}
	if pol.IncludePrompts && pol.IncludeToolContent && pol.Excludes == nil && !pol.RequireClaim {
		return body, sendTally{}
	}
	var msg proto.Message
	switch sig {
	case Logs:
		msg = &logspb.LogsData{}
	case Traces:
		msg = &tracepb.TracesData{}
	default:
		msg = &metricspb.MetricsData{}
	}
	if proto.Unmarshal(body, msg) != nil {
		return nil, sendTally{}
	}
	p := &part{signal: sig, msg: msg}
	if pol.RequireClaim && hasCatchAll(p) {
		return nil, sendTally{}
	}
	tl := sendTally{excluded: dropExcluded(p, pol.Excludes), unclassified: map[string]int{}}
	if tl.excluded > 0 && emptied(msg) {
		return nil, tl
	}
	tl.withheld = r.rules.withhold(p, pol.IncludePrompts, pol.IncludeToolContent, tl.unclassified)
	if tl.withheld == 0 && len(tl.unclassified) == 0 && tl.excluded == 0 {
		return body, tl
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil, sendTally{}
	}
	return out, tl
}
