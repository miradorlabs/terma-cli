package relay

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// testdata/copilot_cli_traces.json is one traces export as Copilot CLI 1.0.96 sent it
// (OTLP/JSON, content capture on), with what was said, its ids and its paths replaced: a
// turn of four requests, two apply_patch calls and a shell command that failed. Every span
// names the session its hooks claim in gen_ai.conversation.id.
const copilotConversation = "00000001-0000-4000-8000-000000000001"

// testdata/copilot_cli_denied_tool_traces.json is another, scrubbed the same way: a turn
// whose one tool call, a file read outside the repository, was denied its permission. It is
// the wire's own account of a failure: a permission span, error.type on the tool's span, an
// error status with a message, and an exception event.
const copilotDeniedConversation = "00000001-0000-4000-8000-000000000002"

// copilotRelay is a relay under pol with the Copilot session claimed or not, the export
// posted as the CLI and VS Code post it: JSON, to an endpoint whose path leads with the
// relay's token, and with no Authorization header.
func copilotRelay(t *testing.T, pol Policy, claimed bool) (*Relay, *upstream, *fixture) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "copilot_cli_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	return copilotRelayOf(t, pol, claimed, copilotConversation, body)
}

// copilotRelayOf is copilotRelay for the export body of the session conversation.
func copilotRelayOf(t *testing.T, pol Policy, claimed bool, conversation string, body []byte) (*Relay, *upstream, *fixture) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	if claimed {
		f.claims[conversation] = claim.Claim{ProjectID: "p1", Tool: "copilot"}
	}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	if code := post(t, srv, "/"+token+"/v1/traces", body, "application/json", "", false); code != http.StatusOK {
		t.Fatalf("the relay answered %d to an export by its path token", code)
	}
	return r, u, f
}

func (u *upstream) sent() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var b strings.Builder
	for _, req := range u.requests {
		b.Write(req.body)
	}
	return b.String()
}

// A claimed Copilot session leaves whole under a policy that collects content.
func TestRelayForwardsAClaimedCopilotSession(t *testing.T) {
	t.Parallel()
	r, u, _ := copilotRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true}, true)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
	sent := u.sent()
	for _, want := range []string{copilotConversation, "PROMPT-TEXT", "REPLY-TEXT", "*** Begin Patch", "env | grep -c OTEL_EXPORTER", "gpt-6-luna"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the forwarded session lacks %q", want)
		}
	}
}

// With content withheld, what was said and what its tools were given and returned stay
// here; the session, its models, token counts, tool names and a command's exit status leave.
func TestRelayWithholdsCopilotContent(t *testing.T) {
	t.Parallel()
	r, u, _ := copilotRelay(t, Policy{}, true)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
	sent := u.sent()
	for _, content := range []string{"PROMPT-TEXT", "REPLY-TEXT", "SYSTEM-INSTRUCTIONS", "TOOL-DEFINITION", "TOOL-DESCRIPTION",
		"Begin Patch", "a.txt", "grep -c", "completed with exit code"} {
		if strings.Contains(sent, content) {
			t.Errorf("content left the machine: %q", content)
		}
	}
	// Every key the CLI sent is one its capture rules name: none was withheld unclassified.
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
	for _, kept := range []string{copilotConversation, "gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens", "gpt-6-luna",
		"apply_patch", "gen_ai.tool.call.id", "github.copilot.interaction_id", "github.copilot.nano_aiu", "process.exit.code", "invoke_agent"} {
		if !strings.Contains(sent, kept) {
			t.Errorf("the forwarded session lacks %q", kept)
		}
	}
}

// A Copilot session no hook claimed is held, then dropped: none of it leaves.
func TestRelayDropsAnUnclaimedCopilotSession(t *testing.T) {
	t.Parallel()
	r, u, f := copilotRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true}, false)
	f.mu.Lock()
	f.now = f.now.Add(DefaultTraceHold + time.Minute)
	f.mu.Unlock()
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unclaimed_expired.traces"] == 8 })
	if sent := u.sent(); sent != "" {
		t.Fatalf("an unclaimed session left the machine: %d bytes", len(sent))
	}
}

// spans are the spans the relay forwarded, by name, and the resources they came under.
func (u *upstream) spans(t *testing.T) (spans map[string]*tracepb.Span, resources []*tracepb.ResourceSpans) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	spans = map[string]*tracepb.Span{}
	for _, req := range u.requests {
		if req.path != "/v1/traces" {
			continue
		}
		var m tracepb.TracesData
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.ResourceSpans {
			resources = append(resources, rs)
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					spans[sp.Name] = sp
				}
			}
		}
	}
	return spans, resources
}

// unclassified are the keys the relay withheld because no rule names them.
func unclassified(r *Relay) []string {
	var keys []string
	for name := range maps.Keys(r.Stats().Snapshot().Counters) {
		if key, ok := strings.CutPrefix(name, "unclassified."); ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// With content withheld, how a call failed still leaves: its code and class, and what the
// permission it was denied was for. The failure's own words, the path the tool was given
// and the status message stay here.
func TestRelayKeepsHowACopilotCallFailed(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("testdata", "copilot_cli_denied_tool_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, u, _ := copilotRelayOf(t, Policy{}, true, copilotDeniedConversation, body)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 5 })
	sent := u.sent()
	for _, content := range []string{"PROMPT-TEXT", "REPLY-TEXT", "SYSTEM-INSTRUCTIONS", "TOOL-DEFINITION", "TOOL-DESCRIPTION",
		"TOOL-RESPONSE", "missing.txt", "STATUS-MESSAGE", "EXCEPTION-MESSAGE"} {
		if strings.Contains(sent, content) {
			t.Errorf("content left the machine: %q", content)
		}
	}
	spans, _ := u.spans(t)
	tool, permission := spans["execute_tool view"], spans["permission"]
	if tool == nil || permission == nil || len(tool.GetEvents()) != 1 {
		t.Fatalf("forwarded spans %q, want the denied tool's with its exception event and the permission's", slices.Sorted(maps.Keys(spans)))
	}
	if tool.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR || tool.GetStatus().GetMessage() != "" {
		t.Errorf("the tool span's status is %v, want an error with no message", tool.GetStatus())
	}
	exception := tool.GetEvents()[0].GetAttributes()
	for _, kept := range []struct{ where, got, want string }{
		{"the tool span's error.type", attr(tool.GetAttributes(), "error.type"), "denied"},
		{"the tool span's session", attr(tool.GetAttributes(), "gen_ai.conversation.id"), copilotDeniedConversation},
		{"the exception's class", attr(exception, "exception.type"), "ToolError"},
		{"the exception's code", attr(exception, "github.copilot.error_code"), "denied"},
		{"the permission's kind", attr(permission.GetAttributes(), "github.copilot.permission.kind"), "read"},
		{"the permission's result", attr(permission.GetAttributes(), "github.copilot.permission.result"), "denied-no-approval-rule-and-could-not-request-from-user"},
	} {
		if kept.got != kept.want {
			t.Errorf("%s left as %q, want %q", kept.where, kept.got, kept.want)
		}
	}
	if got := unclassified(r); !slices.Equal(got, []string{"exception.message"}) {
		t.Errorf("the relay withheld %q as no rule's, want only the exception's message", got)
	}
}

// The agent host VS Code runs the runtime in names itself in service.namespace, and the
// runtime names an MCP tool's server and tool on its span. No capture carries either: they
// are put on the CLI's export here as VS Code's source and the runtime name them. With
// content withheld they leave, since the backend tells the two producers apart by the first.
func TestRelayKeepsWhatNamesCopilotsAgentHost(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "copilot_cli_denied_tool_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]any{"stringValue": value}}
	}
	var export map[string]any
	if err := json.Unmarshal(raw, &export); err != nil {
		t.Fatal(err)
	}
	rs := export["resourceSpans"].([]any)[0].(map[string]any)
	resource := rs["resource"].(map[string]any)
	resource["attributes"] = append(resource["attributes"].([]any), text("service.namespace", "vscode.agent-host"))
	for _, span := range rs["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any) {
		if sp := span.(map[string]any); strings.HasPrefix(sp["name"].(string), "execute_tool") {
			sp["attributes"] = append(sp["attributes"].([]any),
				text("github.copilot.tool.parameters.mcp_server_name", "MCP-SERVER"), text("github.copilot.tool.parameters.mcp_tool_name", "MCP-TOOL"))
		}
	}
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	r, u, _ := copilotRelayOf(t, Policy{}, true, copilotDeniedConversation, body)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 5 })
	spans, resources := u.spans(t)
	tool := spans["execute_tool view"]
	if tool == nil || len(resources) == 0 {
		t.Fatalf("forwarded spans %q under %d resources", slices.Sorted(maps.Keys(spans)), len(resources))
	}
	for _, rs := range resources {
		if got := attr(rs.GetResource().GetAttributes(), "service.namespace"); got != "vscode.agent-host" {
			t.Errorf("a resource left with service.namespace %q", got)
		}
	}
	if server, name := attr(tool.GetAttributes(), "github.copilot.tool.parameters.mcp_server_name"), attr(tool.GetAttributes(), "github.copilot.tool.parameters.mcp_tool_name"); server != "MCP-SERVER" || name != "MCP-TOOL" {
		t.Errorf("the tool span left naming MCP server %q and tool %q", server, name)
	}
}

// copilotExportNaming is the CLI's export with its resource naming a user of its own, as
// nothing of Copilot's does today: a spoof the relay must not pass on as the signed-in user.
func copilotExportNaming(t *testing.T, id, email string) []byte {
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
			map[string]any{"key": semconv.TermaUserIDKey, "value": map[string]any{"stringValue": id}},
			map[string]any{"key": semconv.UserEmailKey, "value": map[string]any{"stringValue": email}})
	}
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Copilot names no one the platform can key a person on, so the relay names the user its
// resolver found: by id under any policy, by email only with content collected. Whatever user
// the export named goes first, and a part the relay names no one on names none at all.
func TestRelayNamesTheSignedInUserOnCopilot(t *testing.T) {
	t.Parallel()
	user := User{ID: "user-a", Email: "a@example.com"}
	for _, tc := range []struct {
		name      string
		pol       Policy
		id, email string
	}{
		{name: "collected", pol: Policy{IncludePrompts: true, IncludeToolContent: true, Anonymous: true, User: user}, id: "user-a", email: "a@example.com"},
		{name: "prompts withheld", pol: Policy{IncludeToolContent: true, Anonymous: true, User: user}, id: "user-a"},
		{name: "content withheld", pol: Policy{Anonymous: true, User: user}, id: "user-a"},
		{name: "no one signed in", pol: Policy{IncludePrompts: true, IncludeToolContent: true, Anonymous: true}},
		// Not an anonymous agent's: the relay names no one, and keeps the agent's own email.
		{name: "an agent that names its user", pol: Policy{IncludePrompts: true, IncludeToolContent: true, User: user}, email: "SPOOF@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, u, _ := copilotRelayOf(t, tc.pol, true, copilotConversation, copilotExportNaming(t, "SPOOF-ID", "SPOOF@example.com"))
			waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
			_, resources := u.spans(t)
			if len(resources) == 0 {
				t.Fatal("no resource left")
			}
			for _, rs := range resources {
				attrs := rs.GetResource().GetAttributes()
				if id, email := attr(attrs, semconv.TermaUserIDKey), attr(attrs, semconv.UserEmailKey); id != tc.id || email != tc.email {
					t.Errorf("a resource left naming %q <%s>, want %q <%s>", id, email, tc.id, tc.email)
				}
			}
			if sent := u.sent(); strings.Contains(sent, "SPOOF-ID") {
				t.Error("the export's own terma.user.id left the machine")
			}
		})
	}
}

// A queued part is checked again against the policy when it is sent: a stricter one by then
// takes the email, never the id the relay named.
func TestQueuedCopilotPartKeepsItsUserUnderAStricterPolicy(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}}}}}
	nameUser(p, Policy{IncludePrompts: true, IncludeToolContent: true, Anonymous: true, User: User{ID: "user-a", Email: "a@example.com"}})
	body, err := proto.Marshal(p.msg)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := r.withholdQueued(Traces, body, Policy{})
	var sent tracepb.TracesData
	if err := proto.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	attrs := sent.ResourceSpans[0].GetResource().GetAttributes()
	if id, email := attr(attrs, semconv.TermaUserIDKey), attr(attrs, semconv.UserEmailKey); id != "user-a" || email != "" {
		t.Errorf("the queued part left naming %q <%s>, want user-a and no email", id, email)
	}
}
