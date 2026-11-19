package relay

import (
	"slices"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A claimed session's records name the credential its hooks read, under any content policy:
// A's with content collected, B's with it withheld. E's hooks could not tell, so its records
// name none. Whatever the agent named itself under the key is gone.
func TestRelayStampsTheCredential(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claim("A", claim.Claim{ProjectID: "p1", Tool: "claude-code", Credential: semconv.TermaAccountCredentialCloud})
	f.claim("B", claim.Claim{ProjectID: "p2", Tool: "claude-code", Credential: semconv.TermaAccountCredentialApiKey})
	f.claim("E", claim.Claim{ProjectID: "p4", Tool: "claude-code"})
	policies := allPolicies(u)
	policies["p4"] = Policy{Endpoint: u.srv.URL, Key: "key-p4", IncludePrompts: true, IncludeToolContent: true}
	r, srv := f.relay(t, u, policies)
	for _, session := range []string{"A", "B", "E"} {
		m := logsOf(session, 1)
		m.ResourceLogs[0].Resource.Attributes = []*commonpb.KeyValue{kv(semconv.TermaAccountCredentialKey, "agent-said")}
		postProto(t, srv, "/v1/logs", m)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 3 })
	u.checkStamps(t, map[string]map[string]string{semconv.TermaAccountCredentialKey: {
		"Bearer key-p1": semconv.TermaAccountCredentialCloud, "Bearer key-p2": semconv.TermaAccountCredentialApiKey, "Bearer key-p4": "",
	}})
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
}

// stampCredential names only what the claim does, and never what the agent sent.
func TestStampCredentialNamesOnlyWhatTheClaimDoes(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"", semconv.TermaAccountCredentialApiKey, semconv.TermaAccountCredentialCloud, semconv.TermaAccountCredentialAccount} {
		p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent"), kv(semconv.TermaAccountCredentialKey, "agent-said")}},
		}}}}
		stampCredential(p, claim.Claim{ProjectID: "p", Credential: want})
		res := p.msg.(*logspb.LogsData).ResourceLogs[0].Resource.Attributes
		named := slices.IndexFunc(res, func(kv *commonpb.KeyValue) bool { return kv.GetKey() == semconv.TermaAccountCredentialKey }) >= 0
		if got := attr(res, semconv.TermaAccountCredentialKey); got != want || named != (want != "") {
			t.Errorf("claim naming %q: stamped %q (present %v)", want, got, named)
		}
	}
}

// The credential is safe on the resource: the field catalog says so, and a queued part checked
// again against a stricter policy as it is sent keeps it.
func TestTheCredentialLeavesUnderAnyPolicy(t *testing.T) {
	t.Parallel()
	if got := Classify(testCapturers, []FieldQuery{{Site: SiteResource, Key: semconv.TermaAccountCredentialKey}}); got[0].Class != FieldSafe {
		t.Errorf("classified %s, want safe", got[0].Class)
	}
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}}}}}
	stampCredential(p, claim.Claim{Credential: semconv.TermaAccountCredentialApiKey})
	body, err := proto.Marshal(p.msg)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := r.withholdQueued(Traces, body, Policy{})
	var sent tracepb.TracesData
	if err := proto.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	if got := attr(sent.ResourceSpans[0].GetResource().GetAttributes(), semconv.TermaAccountCredentialKey); got != semconv.TermaAccountCredentialApiKey {
		t.Errorf("the queued part left naming %q, want api_key", got)
	}
}
