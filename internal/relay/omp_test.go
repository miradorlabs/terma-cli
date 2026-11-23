package relay

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// testdata/omp_traces.json is the trace export omp 19.0.1's native exporter sent for one
// prompt (OTLP/JSON, content capture full), with every content value replaced by
// CONTENT<key>: an edit that failed twice and then landed, a write, a read, and a task
// tool whose subagent ran a bash command. The subagent's spans carry its own session in
// gen_ai.conversation.id, in the parent's trace.
const (
	ompParent = "00000002-0000-7000-8000-000000000001"
	ompChild  = "00000002-0000-7000-8000-000000000002"
	ompSpans  = 17
)

// ompRelay is a relay under pol with omp's session and its subagent's claimed, the
// subagent's claim naming its parent as its start hook does, and the export posted.
func ompRelay(t *testing.T, pol Policy) (*Relay, *upstream) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "omp_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	u := newUpstream(t)
	f := newFixture()
	f.claims[ompParent] = claim.Claim{ProjectID: "p1", Tool: "omp"}
	f.claims[ompChild] = claim.Claim{ProjectID: "p1", Tool: "omp", Parent: ompParent}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, HomeDir: "/home/dev",
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	if code := post(t, srv, "/v1/traces", body, "application/json", token, false); code != http.StatusOK {
		t.Fatalf("the relay answered %d to omp's export", code)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == ompSpans })
	return r, u
}

// parentsBySession is the terma.session.parent.id each forwarded span's resource carried,
// by the span's session.
func (u *upstream) parentsBySession(t *testing.T) map[string][]string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string][]string{}
	for _, req := range u.requests {
		var m tracepb.TracesData
		if req.path != "/v1/traces" {
			continue
		}
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rs := range m.ResourceSpans {
			parent := attr(rs.Resource.Attributes, semconv.TermaSessionParentIDKey)
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					session := attr(sp.Attributes, "gen_ai.conversation.id")
					out[session] = append(out[session], parent)
				}
			}
		}
	}
	return out
}

// With content withheld, omp's prompts, replies, tool calls and their input and output stay
// here; its sessions, models, tokens, cost and each tool call's outcome leave, and every key
// it sends is one a rule names.
func TestRelayWithholdsOmpContent(t *testing.T) {
	t.Parallel()
	r, u := ompRelay(t, Policy{})
	sent := u.sent()
	if strings.Contains(sent, "CONTENT<") {
		t.Errorf("content left the machine: %q", sent[strings.Index(sent, "CONTENT<"):][:60])
	}
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
	for _, kept := range []string{ompParent, ompChild, "gen_ai.usage.input_tokens", "omp.gen_ai.cost.estimated_usd", "omp.gen_ai.tool.status",
		"gen_ai.tool.call.id", "gen_ai.response.id", "mistralai/mistral-small-2603", "execute_tool edit", "invoke_agent task"} {
		if !strings.Contains(sent, kept) {
			t.Errorf("the forwarded session lacks %q", kept)
		}
	}
}

// With prompts collected and tool content withheld, a reply leaves; a tool call's input
// does not, nor the request that restates every tool's result.
func TestRelayWithholdsOmpToolContent(t *testing.T) {
	t.Parallel()
	_, u := ompRelay(t, Policy{IncludePrompts: true})
	sent := u.sent()
	for _, gone := range []string{"CONTENT<omp.gen_ai.response.tool_calls>", "CONTENT<omp.gen_ai.request.messages>", "CONTENT<gen_ai.tool.call.arguments>"} {
		if strings.Contains(sent, gone) {
			t.Errorf("tool content left the machine: %s", gone)
		}
	}
	if !strings.Contains(sent, "CONTENT<omp.gen_ai.response.text>") {
		t.Error("the reply did not leave though prompts are collected")
	}
}

// The relay names its home directory only on the parts of an agent that writes paths from
// `~` (shape.CaptureRules.HomeRelativeScopes): omp's carry none, tool content or not, so what
// is stored for an agent that never needed it does not change.
func TestRelayStampsNoHomeDirectoryOnOmp(t *testing.T) {
	t.Parallel()
	_, u := ompRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true})
	if sent := u.sent(); strings.Contains(sent, semconv.TermaHomeDirectoryKey) || !strings.Contains(sent, semconv.MiradorProjectIDKey) {
		t.Errorf("omp's parts name the home directory, or carry no stamp at all")
	}
}

// A subagent's records carry the session that opened it, from its claim; its parent's carry
// none.
func TestRelayStampsTheParentSession(t *testing.T) {
	t.Parallel()
	_, u := ompRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true})
	parents := u.parentsBySession(t)
	if len(parents[ompChild]) == 0 || len(parents[ompParent]) == 0 {
		t.Fatalf("spans by session: %v", parents)
	}
	for session, want := range map[string]string{ompChild: ompParent, ompParent: ""} {
		for _, got := range parents[session] {
			if got != want {
				t.Errorf("a span of %s carried parent %q, want %q", session, got, want)
			}
		}
	}
}
