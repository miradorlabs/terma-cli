package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// copilotExportNamingUser is Copilot CLI's export with its resource naming a terma.user.id of
// its own, which no agent sends: the relay must not pass it on as the signed-in user.
func copilotExportNamingUser(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "copilot_cli_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	var export map[string]any
	if err := json.Unmarshal(raw, &export); err != nil {
		t.Fatal(err)
	}
	for _, rs := range export["resourceSpans"].([]any) {
		resource := rs.(map[string]any)["resource"].(map[string]any)
		resource["attributes"] = append(resource["attributes"].([]any),
			map[string]any{"key": semconv.TermaUserIDKey, "value": map[string]any{"stringValue": id}})
	}
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Every part the relay sends names the user its policy names, by id, under any content policy,
// and none of the agent's: a part it names no one on names no one at all.
func TestRelayNamesTheUserOnEveryPart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pol  Policy
		want string
	}{
		{name: "collected", pol: Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a"}, want: "user-a"},
		{name: "content withheld", pol: Policy{User: "user-a"}, want: "user-a"},
		{name: "no one signed in", pol: Policy{IncludePrompts: true, IncludeToolContent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, u, _ := copilotRelayOf(t, tc.pol, true, copilotConversation, copilotExportNamingUser(t, "SPOOF-ID"))
			waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
			_, resources := u.spans(t)
			if len(resources) == 0 {
				t.Fatal("no resource left")
			}
			for _, rs := range resources {
				if got := attr(rs.GetResource().GetAttributes(), semconv.TermaUserIDKey); got != tc.want {
					t.Errorf("a resource left naming %q, want %q", got, tc.want)
				}
			}
			if strings.Contains(u.sent(), "SPOOF-ID") {
				t.Error("the export's own terma.user.id left the machine")
			}
			if got := unclassified(r); len(got) > 0 {
				t.Errorf("the relay withheld %q as no rule's", got)
			}
		})
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
