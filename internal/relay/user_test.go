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
// keeps the user the relay named.
func TestQueuedPartKeepsItsUserUnderAStricterPolicy(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}}}}}
	nameUser(p, Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a", UserAccount: "account-a"}, "account-a")
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

// A part names the policy's user only when it leaves under that user's account: one that
// arrived under another person, or under no one (a server key's), and is placed after a
// switch names no one, so its terma.user.id is never another than the sign-in it leaves
// with.
func TestAPartNamesOnlyItsOwnAccountsUser(t *testing.T) {
	t.Parallel()
	pol := Policy{User: "user-b", UserAccount: "account-b"}
	for _, c := range []struct {
		account, want string
	}{
		{account: "account-b", want: "user-b"},
		{account: "account-a"},
		{account: ""}, // a server key's part
	} {
		m := logsOf("A", 1)
		p := &part{msg: m, account: c.account}
		nameUser(p, pol, p.account)
		got := ""
		for _, a := range m.ResourceLogs[0].Resource.GetAttributes() {
			if a.Key == semconv.TermaUserIDKey {
				got = a.Value.GetStringValue()
			}
		}
		if got != c.want {
			t.Errorf("%+v names %q, want %q", c, got, c.want)
		}
	}
}
