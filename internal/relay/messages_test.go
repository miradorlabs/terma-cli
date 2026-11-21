package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The envelopes of one OMP 19.0.1 chat span in a tool loop, in the shape its native exporter
// sends them, with what was said and the tool's input and output replaced by markers: the
// request restates the prompt, the model's reasoning and tool call, and the tool's result; the
// response reasons, replies and calls the tool again. omp.gen_ai.request.messages restates the
// same in OMP's own role/content shape.
const (
	ompInputEnvelope = `[{"role":"user","parts":[{"type":"text","content":"PROMPT<text>"}]},` +
		`{"role":"assistant","parts":[{"type":"reasoning","content":"REASONING<in>"},{"type":"tool_call","id":"call_1","name":"bash","arguments":{"command":"TOOL<arguments-in>"}}]},` +
		`{"role":"tool","name":"bash","parts":[{"type":"tool_call_response","id":"call_1","response":{"content":[{"type":"text","content":"TOOL<result>"}]}}]}]`
	ompOutputEnvelope = `[{"role":"assistant","finish_reason":"tool_call","parts":[{"type":"reasoning","content":"REASONING<out>"},{"type":"text","content":"REPLY<text>"},` +
		`{"type":"tool_call","id":"call_2","name":"bash","arguments":{"command":"TOOL<arguments-out>"}}]}]`
	ompRequestMessages = `[{"role":"user","content":"PROMPT<request>"},{"role":"toolResult","content":"TOOL<request-result>"}]`
)

// With prompts collected and tool content withheld, an OMP chat's envelopes keep what was said
// (the prompt, the reply and the model's reasoning) and lose every tool call's arguments and
// result, each message keeping its role so the turn's shape survives. omp.gen_ai.request.messages,
// whose shape is OMP's own, leaves only when both are collected; with prompts withheld no
// envelope leaves.
func TestRelayWithholdsToolPartsOfOmpMessageEnvelopes(t *testing.T) {
	t.Parallel()
	said := []string{"PROMPT<text>", "REASONING<in>", "REASONING<out>", "REPLY<text>"}
	tool := []string{"TOOL<arguments-in>", "TOOL<result>", "TOOL<arguments-out>"}
	for _, c := range []struct {
		name                      string
		pol                       Policy
		said, tool, requestLeaves bool
	}{
		{"prompts and tool content", Policy{IncludePrompts: true, IncludeToolContent: true}, true, true, true},
		{"prompts only", Policy{IncludePrompts: true}, true, false, false},
		{"tool content only", Policy{IncludeToolContent: true}, false, false, false},
		{"neither", Policy{}, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sent, attrs := relayOmpChat(t, c.pol)
			for _, marker := range said {
				if strings.Contains(sent, marker) != c.said {
					t.Errorf("%s in the export: %v, want %v", marker, !c.said, c.said)
				}
			}
			for _, marker := range tool {
				if strings.Contains(sent, marker) != c.tool {
					t.Errorf("%s in the export: %v, want %v", marker, !c.tool, c.tool)
				}
			}
			if leaves := strings.Contains(sent, "TOOL<request-result>"); leaves != c.requestLeaves {
				t.Errorf("omp.gen_ai.request.messages in the export: %v, want %v", leaves, c.requestLeaves)
			}
			if !c.said {
				for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "omp.gen_ai.request.messages"} {
					if _, ok := attrs[key]; ok {
						t.Errorf("%s left with prompts withheld", key)
					}
				}
				return
			}
			if c.tool {
				if attrs["gen_ai.input.messages"] != ompInputEnvelope || attrs["gen_ai.output.messages"] != ompOutputEnvelope {
					t.Errorf("an envelope changed with all content collected: %v", attrs)
				}
				return
			}
			if got := envelopeRoles(t, attrs["gen_ai.input.messages"]); !slices.Equal(got, []string{"user", "assistant", "tool"}) {
				t.Errorf("input roles = %q, want each message kept", got)
			}
			if got := envelopeRoles(t, attrs["gen_ai.output.messages"]); !slices.Equal(got, []string{"assistant"}) {
				t.Errorf("output roles = %q", got)
			}
		})
	}
}

// The same leak through Copilot's envelopes: a denied file read's path and its tool's response
// stay here with prompts collected and tool content withheld, and the prompt and reply leave.
func TestRelayWithholdsToolPartsOfCopilotMessageEnvelopes(t *testing.T) {
	t.Parallel()
	_, u, _ := copilotRelayOf(t, Policy{IncludePrompts: true}, true, copilotDeniedConversation, readTestdata(t, "copilot_cli_denied_tool_traces.json"))
	waitFor(t, func() bool { return strings.Contains(u.sent(), "REPLY-TEXT") })
	sent := u.sent()
	for _, gone := range []string{"/work/missing.txt", "TOOL-RESPONSE"} {
		if strings.Contains(sent, gone) {
			t.Errorf("tool content left the machine: %s", gone)
		}
	}
	if !strings.Contains(sent, "PROMPT-TEXT") {
		t.Error("the prompt did not leave though prompts are collected")
	}
}

// An envelope the relay cannot read as one, or a part it does not know as what was said, is
// withheld: whole for the first, the part for the second. A second pass changes nothing.
func TestMessageEnvelopeWithholdingFailsClosed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		in   *commonpb.AnyValue
		want string // "" when the attribute is withheld whole
	}{
		{"not JSON", strValue("PROMPT<text> TOOL<result>"), ""},
		{"an object", strValue(`{"role":"user","parts":[]}`), ""},
		{"a message without parts", strValue(`[{"role":"tool","content":"TOOL<result>"}]`), ""},
		{"parts not a list", strValue(`[{"role":"tool","parts":"TOOL<result>"}]`), ""},
		{"not a string", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
			Values: []*commonpb.AnyValue{strValue("TOOL<result>")}}}}, ""},
		{"an unknown part type", strValue(`[{"role":"tool","parts":[{"type":"tool_call_3","id":"c","response":"TOOL<result>"}]}]`),
			`[{"role":"tool","parts":[]}]`},
		{"a text part with more than its text", strValue(`[{"role":"user","parts":[{"type":"text","content":"PROMPT<text>","arguments":"TOOL<x>"}]}]`),
			`[{"role":"user","parts":[]}]`},
		{"a text part whose content is no string", strValue(`[{"role":"user","parts":[{"type":"text","content":{"path":"TOOL<x>"}}]}]`),
			`[{"role":"user","parts":[]}]`},
		{"a repeated key", strValue(`[{"role":"user","parts":[{"type":"text","content":{"path":"TOOL<x>"},"content":"PROMPT<text>"}]}]`),
			`[{"role":"user","parts":[]}]`},
		{"a role that is no string", strValue(`[{"role":{"tool":"TOOL<x>"},"parts":[]}]`), ""},
		{"a tool's output as text", strValue(`[{"role":"tool","name":"bash","parts":[{"type":"text","content":"TOOL<result>"}]}]`),
			`[{"role":"tool","name":"bash","parts":[]}]`},
		{"a tool role in capitals", strValue(`[{"role":"Tool","parts":[{"type":"text","content":"TOOL<result>"}]}]`),
			`[{"role":"Tool","parts":[]}]`},
		{"a participant's name", strValue(`[{"role":"user","name":"PROMPT<name>","parts":[{"type":"text","content":"PROMPT<text>"}]}]`),
			`[{"role":"user","parts":[{"type":"text","content":"PROMPT<text>"}]}]`},
		{"a message key no rule names", strValue(`[{"role":"user","tool_calls":"TOOL<x>","parts":[{"type":"text","content":"PROMPT<a&b>"}]}]`),
			`[{"role":"user","parts":[{"type":"text","content":"PROMPT<a&b>"}]}]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			attrs := []*commonpb.KeyValue{{Key: "gen_ai.input.messages", Value: c.in}}
			out, changed := testRules.withholdAttrs(attrs, true, false, false, nil, map[string]int{})
			if !changed {
				t.Fatal("the pass reported no change")
			}
			got := ""
			if len(out) > 0 {
				got = out[0].GetValue().GetStringValue()
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if got == "" {
				return
			}
			again, changed := testRules.withholdAttrs(out, true, false, false, nil, map[string]int{})
			if changed || again[0].GetValue().GetStringValue() != got {
				t.Errorf("a second pass changed %q to %q", got, again[0].GetValue().GetStringValue())
			}
		})
	}
}

// relayOmpChat sends one OMP chat span carrying the envelopes through a relay under pol and
// returns the bytes upstream received and the forwarded span's string attributes.
func relayOmpChat(t *testing.T, pol Policy) (string, map[string]string) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	f.claims[ompParent] = claim.Claim{ProjectID: "p1", Tool: "omp"}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	sp := &tracepb.Span{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Name: "chat mistralai/mistral-small-2603",
		Attributes: []*commonpb.KeyValue{kv("gen_ai.conversation.id", ompParent), kv("gen_ai.operation.name", "chat"),
			kv("gen_ai.input.messages", ompInputEnvelope), kv("gen_ai.output.messages", ompOutputEnvelope),
			kv("omp.gen_ai.request.messages", ompRequestMessages)}}
	sp.TraceId[0] = 1
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/traces", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("the relay answered %d", code)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 1 })
	sent := u.sent()
	u.mu.Lock()
	defer u.mu.Unlock()
	attrs := map[string]string{}
	for _, req := range u.requests {
		var m tracepb.TracesData
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					for _, a := range sp.Attributes {
						attrs[a.Key] = a.GetValue().GetStringValue()
					}
				}
			}
		}
	}
	return sent, attrs
}

// envelopeRoles is the role of each message in raw, which must be an envelope.
func envelopeRoles(t *testing.T, raw string) []string {
	t.Helper()
	var msgs []struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatalf("the envelope is no longer JSON: %v: %q", err, raw)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	return roles
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}
