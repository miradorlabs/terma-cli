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

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// testdata/pi_export/ is what terma's Pi extension (wire version 2) posted to the relay, one
// file per case, every content switch of the extension on: a prompt with a write, an edit and
// a failing command (tools); a message an extension put into the conversation (custom-message);
// a compaction (compaction); a provider that refused the call (api-error); a child pi a bash
// call started, linked to its parent (subagent). Each case's requests are posted in order, its
// sessions claimed, a child's naming its parent as its start hook does.
var piCases = []string{"tools", "custom-message", "compaction", "api-error", "subagent"}

const piScope = "terma-pi"

type piCase struct {
	paths    []string
	bodies   [][]byte
	spans    int
	records  int
	sessions map[string]string // session → its parent, "" for a root
	sent     map[string]bool   // every attribute key the extension sent
	events   map[string]bool   // every log record's event.name
}

func loadPiCase(t *testing.T, name string) piCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "pi_export", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Requests []struct {
			Path string          `json:"path"`
			Body json.RawMessage `json:"body"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	c := piCase{sessions: map[string]string{}, sent: map[string]bool{}, events: map[string]bool{}}
	note := func(attrs []*commonpb.KeyValue) {
		for _, kv := range attrs {
			c.sent[kv.Key] = true
		}
		if s := attr(attrs, "session.id"); s != "" {
			c.sessions[s] = attr(attrs, "pi.parent_session.id")
		}
	}
	for _, req := range file.Requests {
		c.paths = append(c.paths, req.Path)
		c.bodies = append(c.bodies, req.Body)
		switch req.Path {
		case "/v1/traces":
			var m tracepb.TracesData
			if err := protojson.Unmarshal(req.Body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rs := range m.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						c.spans++
						note(sp.Attributes)
					}
				}
			}
		case "/v1/logs":
			var m logspb.LogsData
			if err := protojson.Unmarshal(req.Body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rl := range m.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						c.records++
						note(lr.Attributes)
						c.events[attr(lr.Attributes, "event.name")] = true
					}
				}
			}
		}
	}
	return c
}

// piRelay is a relay under pol with the case's sessions claimed and its export posted.
func piRelay(t *testing.T, c piCase, pol Policy) (*Relay, *upstream) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	for session, parent := range c.sessions {
		f.claims[session] = claim.Claim{ProjectID: "p1", Tool: "pi", Parent: parent, Root: "/work/repo", Cwd: "/work/repo"}
	}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	for i, path := range c.paths {
		if code := post(t, srv, path, c.bodies[i], "application/json", token, false); code != http.StatusOK {
			t.Fatalf("the relay answered %d to %s", code, path)
		}
	}
	waitForCounters(t, r, func(n map[string]int) bool {
		return n["forwarded.traces"] == c.spans && n["forwarded.logs"] == c.records
	})
	return r, u
}

// piForwarded is what upstream received: every attribute key with each value it carried,
// and the bodies of the log records by event.
type piForwarded struct {
	values map[string][]*commonpb.AnyValue
	bodies map[string][]string
}

func (u *upstream) pi(t *testing.T) piForwarded {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := piForwarded{values: map[string][]*commonpb.AnyValue{}, bodies: map[string][]string{}}
	note := func(scope string, attrs []*commonpb.KeyValue) {
		if scope != piScope {
			t.Errorf("a record left under scope %q", scope)
		}
		for _, kv := range attrs {
			out.values[kv.Key] = append(out.values[kv.Key], kv.Value)
		}
	}
	for _, req := range u.requests {
		switch req.path {
		case "/v1/traces":
			var m tracepb.TracesData
			if err := proto.Unmarshal(req.body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rs := range m.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						note(ss.Scope.GetName(), sp.Attributes)
					}
				}
			}
		case "/v1/logs":
			var m logspb.LogsData
			if err := proto.Unmarshal(req.body, &m); err != nil {
				t.Fatal(err)
			}
			for _, rl := range m.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						note(sl.Scope.GetName(), lr.Attributes)
						event := attr(lr.Attributes, "event.name")
						out.bodies[event] = append(out.bodies[event], lr.Body.GetStringValue())
					}
				}
			}
		}
	}
	return out
}

// The keys the cases between them must carry through any policy (a turn, a call and a parent
// link must never be lost to withholding), and the content each switch governs. A key a case
// did not send is not looked for in it; piSafeIfSent are safe keys no captured case needs to
// send (a fork's source, an MCP tool's names).
var (
	piSafe = []string{"session.id", "pi.version", "pi.mode", "pi.parent_session.id", "pi.turn.id", "pi.call.id", "pi.call.kind",
		"pi.entry.id", "pi.thinking_level", "pi.compaction.reason", "pi.compaction.tokens_before", "pi.tool.outcome", "pi.tool.exit_code",
		"pi.prompt.length", "pi.completion.length", "pi.cost.from_provider", "gen_ai.tool.call.id", "gen_ai.tool.name",
		"gen_ai.request.model", "gen_ai.provider.name", "gen_ai.usage.input_tokens", "gen_ai.usage.total_cost",
		"gen_ai.response.finish_reasons", "event.name"}
	piSafeIfSent  = []string{"pi.session.forked_from.id", "pi.mcp.server", "pi.mcp.tool", "pi.auth.oauth"}
	piPromptKeys  = []string{"gen_ai.completion"}
	piToolKeys    = []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "pi.tool.files"}
	piBothKeys    = []string{"pi.error.message", "pi.compaction.summary"}
	piPromptBody  = []string{"pi.user_prompt", "pi.session.title"}
	piBothBody    = []string{"pi.custom_message"}
	piMustRecords = []string{"pi.user_prompt", "pi.session.title", "pi.custom_message"}
)

// Under each content policy every case's keys are classified, what the policy collects
// arrives, the content it withholds stays here, and the ids, counts and names that place
// and measure a call leave whatever it says. The prompt and the title follow the prompt
// switch alone; a custom message, an error and a compaction summary leave only with both.
func TestRelayAppliesEachPolicyToThePiExport(t *testing.T) {
	t.Parallel()
	sent, events := map[string]bool{}, map[string]bool{}
	for _, name := range piCases {
		c := loadPiCase(t, name)
		maps.Copy(sent, c.sent)
		maps.Copy(events, c.events)
	}
	for _, k := range slices.Concat(piSafe, piPromptKeys, piToolKeys, piBothKeys) {
		if !sent[k] {
			t.Errorf("no captured case sends %q: the test would prove nothing of it", k)
		}
	}
	for _, e := range piMustRecords {
		if !events[e] {
			t.Errorf("no captured case sends a %s record", e)
		}
	}
	for _, name := range piCases {
		for _, pol := range []Policy{{}, {IncludePrompts: true}, {IncludeToolContent: true}, {IncludePrompts: true, IncludeToolContent: true}} {
			t.Run(name+policyName(pol), func(t *testing.T) {
				t.Parallel()
				c := loadPiCase(t, name)
				r, u := piRelay(t, c, pol)
				for counter := range r.Stats().Snapshot().Counters {
					if strings.HasPrefix(counter, "unclassified.") {
						t.Errorf("the relay withheld a key no rule names: %s", counter)
					}
				}
				got := u.pi(t)
				arrives := func(keys []string, want bool) {
					for _, k := range keys {
						if !c.sent[k] {
							continue
						}
						if _, ok := got.values[k]; ok != want {
							t.Errorf("%s: arrived %v, want %v", k, ok, want)
						}
					}
				}
				arrives(piSafe, true)
				arrives(piSafeIfSent, true)
				arrives(piPromptKeys, pol.IncludePrompts)
				arrives(piToolKeys, pol.IncludeToolContent)
				arrives(piBothKeys, pol.IncludePrompts && pol.IncludeToolContent)
				bodies := func(events []string, want bool) {
					for _, event := range events {
						for _, body := range got.bodies[event] {
							if (body != "") != want {
								t.Errorf("%s body %q, want sent %v", event, body, want)
							}
						}
					}
				}
				bodies(piPromptBody, pol.IncludePrompts)
				bodies(piBothBody, pol.IncludePrompts && pol.IncludeToolContent)
				if pol.IncludePrompts && pol.IncludeToolContent {
					for k := range c.sent {
						if _, ok := got.values[k]; !ok {
							t.Errorf("with every switch on, %s did not arrive", k)
						}
					}
				}
			})
		}
	}
}

// In global mode a Pi session's spans and records wait for the extension's claim rather than
// going to the default project: the extension claims every session before it exports any of
// it. Unclaimed, they are dropped, never sent.
func TestRelayHoldsAnUnclaimedPiSessionInGlobalMode(t *testing.T) {
	t.Parallel()
	c := loadPiCase(t, "tools")
	for _, claimed := range []bool{true, false} {
		u := newUpstream(t)
		f := newFixture()
		pol := Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
		r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
			CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
			Resolve:  func(claim.Claim) (Policy, error) { return pol, nil }})
		runRelay(t, r)
		srv := httptest.NewServer(r.Handler())
		for i, path := range c.paths {
			post(t, srv, path, c.bodies[i], "application/json", token, false)
		}
		waitForCounters(t, r, func(n map[string]int) bool { return n["received.traces"] > 0 && n["received.logs"] > 0 })
		if len(u.pi(t).values) > 0 {
			t.Error("an unclaimed session's records left before its claim")
		}
		if claimed {
			f.mu.Lock()
			for session, parent := range c.sessions {
				f.claims[session] = claim.Claim{ProjectID: "p-default", Tool: "pi", Parent: parent}
			}
			f.mu.Unlock()
			waitForCounters(t, r, func(n map[string]int) bool {
				return n["forwarded.traces"] == c.spans && n["forwarded.logs"] == c.records
			})
		} else {
			f.mu.Lock()
			f.now = f.now.Add(DefaultTraceHold + time.Minute)
			f.mu.Unlock()
			waitForCounters(t, r, func(n map[string]int) bool {
				return n["dropped.unclaimed_expired.traces"] == c.spans && n["dropped.unclaimed_expired.logs"] == c.records
			})
			if len(u.pi(t).values) > 0 {
				t.Error("an unclaimed session's records left")
			}
		}
		srv.Close()
	}
}
