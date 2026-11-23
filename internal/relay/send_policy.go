package relay

import (
	"bytes"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// sendTally is what applying the policy to a queued body took from it.
type sendTally struct {
	withheld     int
	unclassified map[string]int
}

// count records what the policy took from a body that was sent.
func (r *Relay) count(sig Signal, tl sendTally) {
	if tl.withheld > 0 {
		r.stats.add("withheld_at_send_records", tl.withheld)
	}
	for key, c := range tl.unclassified {
		r.stats.unclassified(key, c)
	}
}

// withholdQueued applies the policy as it stands now to a queued body of a route in lane
// account, reporting what it took; nil drops all of it, as does a body that no longer
// decodes, since it cannot be checked against a stricter policy. A body that names a user
// names only its lane's own, as nameUser would now: an older relay named its user on every
// part, and a part taken into no person's lane, or held for another person, names no one.
func (r *Relay) withholdQueued(sig Signal, body []byte, pol Policy, account string) ([]byte, sendTally) {
	content := !pol.IncludePrompts || !pol.IncludeToolContent || pol.RequireClaim
	// The key's bytes are in any body that names a user; one without them is left as it is,
	// and so is one in its user's own lane that names that user alone (namesOnly).
	user := bytes.Contains(body, []byte(semconv.TermaUserIDKey)) && !namesOnly(body, pol, account)
	if !content && !user {
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
	p := &part{signal: sig, msg: msg, account: account}
	if pol.RequireClaim && hasCatchAll(p) {
		return nil, sendTally{}
	}
	tl := sendTally{unclassified: map[string]int{}}
	if content {
		tl.withheld = r.rules.withhold(p, pol.IncludePrompts, pol.IncludeToolContent, tl.unclassified)
	}
	if user {
		nameUser(p, pol)
	} else if tl.withheld == 0 && len(tl.unclassified) == 0 {
		return body, tl
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil, sendTally{}
	}
	return out, tl
}

// namesOnly reports, without decoding body, that every terma.user.id in it is pol.User's,
// as stamp encodes it, in pol.User's own lane (nameUser), so naming it again changes
// nothing. Each occurrence of the key's bytes must start the whole encoded pair: a key
// anywhere else, or an id another than pol.User, counts the key more often than the pair,
// and the body is decoded. It errs only toward decoding. It relies on body being one this
// relay marshalled (enqueue, the outbox merge), never an agent's raw bytes: a raw body
// could repeat a KeyValue's value field after the pair.
func namesOnly(body []byte, pol Policy, account string) bool {
	if pol.User == "" || account == "" || account != pol.UserAccount {
		return false
	}
	key := []byte(semconv.TermaUserIDKey)
	pair, err := proto.Marshal(&commonpb.KeyValue{Key: semconv.TermaUserIDKey, Value: strValue(pol.User)})
	if err != nil || bytes.Count(pair, key) != 1 {
		return false
	}
	return bytes.Count(body, key) == bytes.Count(body, pair)
}
