package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// What the agents declare between them, pinned: a change here changes what leaves a machine.
var (
	pinnedPromptFields     = []string{"prompt", "prompt_text", "response", "user_prompt"}
	pinnedPromptDropFields = []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
		"gen_ai.tool.definitions",
		"omp.gen_ai.request.messages", "omp.gen_ai.response.text",
		"request_text", "response_text",
		// Free text that may restate what was said, and keys too generic to vouch for.
		"error", "reason", "reasoning", "routing.reasoning", "metadata", "value", "key", "from", "db", "query_script",
		"gen_ai.tool.description", "gen_ai.agent.description", "gen_ai.request.stop_sequences"}
	pinnedResourcePromptFields = []string{"process.command_args", "process.command_line"}
	pinnedPromptBodyEvents     = []string{"opencode.user_prompt", "opencode.session.created", "pi.user_prompt", "omp.user_prompt", "hermes.user_prompt", "hermes.assistant_response", "dsh.user_prompt", "dsh.assistant_response"}
	pinnedToolContentFields    = []string{"tool_parameters", "tool_input", "full_command", "bash_command", "arguments", "output",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.file_path",
		"function_args", "hook_input", "hook_output", "stdout", "stderr",
		"file_path", "result"}
	pinnedToolContentEvents = []string{"tool.output", "tool.input"}
	pinnedSessionKeys       = []string{"session.id", "conversation.id", "gen_ai.conversation.id", "thread.id", "thread_id"}
)

func TestComposedRulesArePinned(t *testing.T) {
	t.Parallel()
	for _, s := range []struct {
		name      string
		got, want []string
	}{
		{"prompt fields", testRules.promptFields, pinnedPromptFields},
		{"prompt drop fields", testRules.promptDropFields, pinnedPromptDropFields},
		{"prompt body events", testRules.promptBodyEvents, pinnedPromptBodyEvents},
		{"resource prompt fields", testRules.resourcePromptFields, pinnedResourcePromptFields},
		{"tool content fields", testRules.toolContentFields, pinnedToolContentFields},
		{"tool content events", testRules.toolContentEvents, pinnedToolContentEvents},
		{"start events", testRules.startEvents, []string{"codex.conversation_starts"}},
	} {
		if got, want := slices.Sorted(slices.Values(s.got)), slices.Sorted(slices.Values(s.want)); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", s.name, got, want)
		}
	}
	var keys, numericRejected []string
	for _, k := range testRules.sessionKeys {
		keys = append(keys, k.Attr)
		if k.RejectNumeric {
			numericRejected = append(numericRejected, k.Attr)
		}
	}
	if !slices.Equal(keys, pinnedSessionKeys) {
		t.Errorf("session key precedence = %q, want %q", keys, pinnedSessionKeys)
	}
	if !slices.Equal(numericRejected, []string{"thread.id", "thread_id"}) {
		t.Errorf("numeric ids rejected for %q", numericRejected)
	}
}

// A key one agent declares as content is never on the safe list, or another agent's record would carry it out.
func TestNoDeclaredContentKeyIsSafe(t *testing.T) {
	t.Parallel()
	for _, c := range testCapturers {
		r := c.CaptureRules()
		for _, key := range slices.Concat(r.PromptFields, r.PromptDropFields, r.ResourcePromptFields, r.ToolContentFields) {
			if testRules.safeKey(key) {
				t.Errorf("%T declares %q as content, and allow.go lists it safe", c, key)
			}
		}
	}
}

// Every known agent's shape is composed, supported or not: its hooks still claim sessions.
func TestEveryExportingAgentDeclaresItsShape(t *testing.T) {
	t.Parallel()
	reg := builtin.Agents("")
	for _, a := range reg.All() {
		_, correlates := a.(shape.Correlator)
		_, captures := a.(shape.Capturer)
		if correlates != captures {
			t.Errorf("%s declares one half of its telemetry shape", a.Name())
		}
	}
	if n, m := len(testCorrelators), len(reg.With[shape.Capturer]()); n == 0 || n != m {
		t.Fatalf("%d correlators, %d capturers", n, m)
	}
}

// For every agent, mode and content policy, each declared content field leaves only when
// the policy allows it; sentinel values show in the bytes upstream receives.
func TestPolicyMatrixPerAgent(t *testing.T) {
	t.Parallel()
	for _, c := range testCapturers {
		agent := strings.TrimPrefix(fmt.Sprintf("%T", c), "*")
		session := shape.SessionID
		if cor, ok := c.(shape.Correlator); ok && len(cor.Correlation().SessionKeys) > 0 {
			session = cor.Correlation().SessionKeys[0]
		}
		rules := c.CaptureRules()
		for _, global := range []bool{false, true} {
			for _, prompts := range []bool{false, true} {
				for _, toolContent := range []bool{false, true} {
					name := fmt.Sprintf("%s/global=%v/prompts=%v/tools=%v", agent, global, prompts, toolContent)
					t.Run(name, func(t *testing.T) {
						got := deliverThroughRelay(t, global, Policy{IncludePrompts: prompts, IncludeToolContent: toolContent}, session, rules)
						check := func(kind string, keys []string, allowed bool) {
							for _, key := range keys {
								leaked := strings.Contains(got, sentinel(key))
								if kind == "tool content event" {
									leaked = strings.Contains(got, key)
								}
								if leaked != allowed {
									t.Errorf("%s %q: in the export %v, policy allows %v", kind, key, leaked, allowed)
								}
							}
						}
						check("prompt field", rules.PromptFields, prompts)
						check("prompt drop field", rules.PromptDropFields, prompts)
						check("prompt body event", rules.PromptBodyEvents, prompts)
						check("resource prompt field", rules.ResourcePromptFields, prompts)
						check("tool content field", rules.ToolContentFields, toolContent)
						check("tool content event", rules.ToolContentEvents, toolContent)
					})
				}
			}
		}
	}
}

func sentinel(key string) string { return "SENTINEL<" + key + ">" }

// deliverThroughRelay posts every content field in rules through a running relay and returns what upstream received.
func deliverThroughRelay(t *testing.T, global bool, pol Policy, key shape.SessionKey, rules shape.CaptureRules) string {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	pol.Endpoint, pol.Key = u.srv.URL, "key"
	opts := Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }}
	sid, sessionKey := "S", key.Attr
	if global {
		opts.CatchAll = func() (claim.Claim, bool) { return claim.Claim{ProjectID: "team"}, true }
		if key.Claimed {
			// Global mode's catch-all never takes such a session: its hook's claim places it.
			f.claims[sid] = claim.Claim{ProjectID: "team"}
		}
	} else {
		f.claims[sid] = claim.Claim{ProjectID: "p1"}
		pol.RequireClaim = true
	}
	r := newRelay(opts)
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	var attrs []*commonpb.KeyValue
	for _, key := range slices.Concat(rules.PromptFields, rules.PromptDropFields, rules.ToolContentFields) {
		attrs = append(attrs, kv(key, sentinel(key)))
	}
	var resAttrs []*commonpb.KeyValue
	for _, key := range rules.ResourcePromptFields {
		resAttrs = append(resAttrs, kv(key, sentinel(key)))
	}
	recs := []*logspb.LogRecord{{Attributes: append([]*commonpb.KeyValue{kv(sessionKey, sid)}, attrs...)}}
	for _, event := range rules.PromptBodyEvents {
		recs = append(recs, &logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv(sessionKey, sid), kv("event.name", event)},
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sentinel(event)}}})
	}
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{Attributes: resAttrs},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
	sp := &tracepb.Span{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Name: "turn",
		Attributes: append([]*commonpb.KeyValue{kv(sessionKey, sid)}, attrs...)}
	sp.TraceId[0] = 1
	for _, event := range rules.ToolContentEvents {
		sp.Events = append(sp.Events, &tracepb.Span_Event{Name: event})
	}
	traces := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: resAttrs},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}

	for path, msg := range map[string]proto.Message{"/v1/logs": logs, "/v1/traces": traces} {
		body, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if code := post(t, srv, path, body, "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("%s = %d", path, code)
		}
	}
	want := len(recs)
	waitForCounters(t, r, func(c map[string]int) bool {
		return c["forwarded.logs"] == want && c["forwarded.traces"] == 1
	})
	u.mu.Lock()
	defer u.mu.Unlock()
	var all strings.Builder
	for _, req := range u.requests {
		all.Write(req.body)
	}
	return all.String()
}

// With content withheld a log body passes only when it names its own event, whole or after a declared prefix.
func TestWithheldBodyOnlyNamesItsEvent(t *testing.T) {
	t.Parallel()
	for body, kept := range map[string]bool{
		"api_request":             true,
		"claude_code.api_request": true,
		"PRIVATE.api_request":     false,
		"api_request PRIVATE":     false,
	} {
		lr := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{kv("event.name", "api_request")},
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}}}
		testRules.withhold(&part{msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{lr}}}}}}}, false, false, map[string]int{})
		if got := lr.Body.GetStringValue() == body; got != kept {
			t.Errorf("body %q kept %v, want %v", body, got, kept)
		}
	}
}

// Each record names its session by the first key in precedence order; a numeric thread
// id is an OS thread and names none. Codex's keys are Claimed: its hooks claim every session.
func TestSessionKeyPrecedence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		attrs   []*commonpb.KeyValue
		want    string
		claimed bool
	}{
		{[]*commonpb.KeyValue{kv("thread.id", "T"), kv("conversation.id", "C"), kv("session.id", "S")}, "S", false},
		{[]*commonpb.KeyValue{kv("thread.id", "T"), kv("gen_ai.conversation.id", "G"), kv("conversation.id", "C")}, "C", true},
		{[]*commonpb.KeyValue{kv("thread_id", "U"), kv("thread.id", "T")}, "T", true},
		{[]*commonpb.KeyValue{kv("thread.id", "4242"), kv("thread_id", "U")}, "U", true},
		{[]*commonpb.KeyValue{kv("thread.id", "4242")}, "", false},
	} {
		if got, claimed := testRules.sessionOf(c.attrs, nil); got != c.want || claimed != c.claimed {
			t.Errorf("%v: session %q claimed %v, want %q %v", c.attrs, got, claimed, c.want, c.claimed)
		}
	}
	if got, _ := testRules.sessionOf(nil, []*commonpb.KeyValue{kv("session.id", "R")}); got != "R" {
		t.Errorf("resource session %q, want R", got)
	}
}
