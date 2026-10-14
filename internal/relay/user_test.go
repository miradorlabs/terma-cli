package relay

import (
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Every part the relay sends names the user its policy names, by id, whichever agent sent it
// and under any content policy, and none of the agent's own: Claude Code's (A, content
// collected) and Codex's (B, content withheld) name the signed-in user; a part whose policy
// names no one (E) names no one at all.
func TestRelayNamesTheUserOnEveryPart(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims["E"] = claim.Claim{ProjectID: "p4", Tool: "claude-code"}
	r, srv := f.relay(t, u, map[string]Policy{
		"p1": {Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true, User: "user-a"},
		"p2": {Endpoint: u.srv.URL, Key: "key-p2", User: "user-a"},
		"p4": {Endpoint: u.srv.URL, Key: "key-p4", IncludePrompts: true, IncludeToolContent: true},
	})
	for _, session := range []string{"A", "B", "E"} {
		m := logsOf(session, 1)
		m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(semconv.TermaUserIDKey, "SPOOF-ID")}
		postProto(t, srv, "/v1/logs", m)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 3 })
	u.checkStamps(t, map[string]map[string]string{semconv.TermaUserIDKey: {
		"Bearer key-p1": "user-a", "Bearer key-p2": "user-a", "Bearer key-p4": "",
	}})
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
}

// A queued part is checked again against the policy when it is sent: a stricter one by then
// keeps the user the relay named.
func TestQueuedPartKeepsItsUserUnderAStricterPolicy(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}}}}}
	nameUser(p, Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a"})
	body, err := proto.Marshal(p.msg)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := r.withholdQueued(Traces, body, Policy{})
	var sent tracepb.TracesData
	if err := proto.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	if got := attr(sent.ResourceSpans[0].GetResource().GetAttributes(), semconv.TermaUserIDKey); got != "user-a" {
		t.Errorf("the queued part left naming %q, want user-a", got)
	}
}
