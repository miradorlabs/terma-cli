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
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// Every part the relay sends names the user its policy names, by id, whichever agent sent it
// and under any content policy, and none of the agent's own: Claude Code's (A, content
// collected) and Codex's (B, content withheld) name the signed-in user; a part whose policy
// names no one (E) names no one at all.
func TestRelayNamesTheUserOnEveryPart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		sessions []string
		want     string
	}{{[]string{"A", "B"}, "user-a"}, {[]string{"E"}, ""}} {
		u := newUpstream(t)
		f := newFixture()
		f.signIn = &testSignIn{account: accountA}
		f.claims["E"] = claim.Claim{ProjectID: "p4", Tool: "claude-code"}
		r, srv := f.relay(t, u, map[string]Policy{
			"p1": {Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true, User: "user-a", UserAccount: accountA},
			"p2": {Endpoint: u.srv.URL, User: "user-a", UserAccount: accountA},
			"p4": {Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true},
		})
		for _, session := range c.sessions {
			m := logsOf(session, 1)
			m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(semconv.TermaUserIDKey, "SPOOF-ID")}
			postProto(t, srv, "/v1/logs", m)
		}
		waitForCounters(t, r, func(n map[string]int) bool { return n["forwarded.logs"] == len(c.sessions) })
		u.checkStamps(t, map[string]map[string]string{semconv.TermaUserIDKey: {"Bearer token-" + accountA: c.want}})
		for name := range r.Stats().Snapshot().Counters {
			if strings.HasPrefix(name, "unclassified.") {
				t.Errorf("the relay withheld a key no rule names: %s", name)
			}
		}
	}
}

// A queued part is checked again against the policy when it is sent: a stricter one by then
// keeps the user the relay named, in that user's own lane.
func TestQueuedPartKeepsItsUserUnderAStricterPolicy(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	p := &part{signal: Traces, account: accountA, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}}}}}
	nameUser(p, Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a", UserAccount: accountA})
	body, err := proto.Marshal(p.msg)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := r.withholdQueued(Traces, body, Policy{User: "user-a", UserAccount: accountA}, accountA)
	var sent tracepb.TracesData
	if err := proto.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	if got := attr(sent.ResourceSpans[0].GetResource().GetAttributes(), semconv.TermaUserIDKey); got != "user-a" {
		t.Errorf("the queued part left naming %q, want user-a", got)
	}
}

// A part names the policy's user only when its lane is that user's account: one that
// arrived under another person, or under no one (a server key's), and is placed after a
// switch names no one, so its terma.user.id is never another person's than the sign-in it
// leaves with. Whatever the agent sent under the key is gone either way.
func TestAPartNamesOnlyItsOwnAccountsUser(t *testing.T) {
	t.Parallel()
	pol := Policy{User: "user-b", UserAccount: accountB}
	for _, c := range []struct {
		account, want string
	}{
		{account: accountB, want: "user-b"},
		{account: accountA},
		{account: sendas.NoPersonLane},
		{account: ""},
	} {
		m := logsOf("A", 1)
		m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(semconv.TermaUserIDKey, "user-b")}
		nameUser(&part{msg: m, account: c.account}, pol)
		if got := attr(m.ResourceLogs[0].Resource.GetAttributes(), semconv.TermaUserIDKey); got != c.want {
			t.Errorf("%+v names %q, want %q", c, got, c.want)
		}
	}
	// A part bound to no lane names no one, even under a policy whose user has no account.
	m := logsOf("A", 1)
	nameUser(&part{msg: m}, Policy{User: "user-b"})
	if got := attr(m.ResourceLogs[0].Resource.GetAttributes(), semconv.TermaUserIDKey); got != "" {
		t.Errorf("a part of no lane names %q", got)
	}
}
