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

// testdata/opencode_export/ is what terma's OpenCode plugin posted to the relay from OpenCode
// 1.19.6, one file per case, every content switch of the plugin's on (README.md there): a
// shell call, a write and an edit (tools); a task tool whose subagent ran in its own session
// (subagent); an MCP tool (mcp); a tool a rule denied and one the person denied (denied-rule,
// denied-ask); a provider that refused the call (api-error). Each case's requests are posted
// in order, its sessions claimed, a subagent's naming its parent as its start hook does.
var opencodeCases = []string{"tools", "subagent", "mcp", "denied-rule", "denied-ask", "api-error"}

const opencodeScope = "terma-opencode"

type opencodeCase struct {
	name     string
	paths    []string
	bodies   [][]byte
	spans    int
	records  int
	sessions map[string]string // session → its parent, "" for a root
	sent     map[string]bool   // every attribute key the plugin sent
}

func loadOpenCodeCase(t *testing.T, name string) opencodeCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "opencode_export", name+".json"))
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
	c := opencodeCase{name: name, sessions: map[string]string{}, sent: map[string]bool{}}
	note := func(attrs []*commonpb.KeyValue) {
		for _, kv := range attrs {
			c.sent[kv.Key] = true
		}
		if s := attr(attrs, "session.id"); s != "" {
			c.sessions[s] = attr(attrs, "opencode.parent_session.id")
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
					}
				}
			}
		}
	}
	return c
}

// opencodeRelay is a relay under pol with the case's sessions claimed and its export posted.
func opencodeRelay(t *testing.T, c opencodeCase, pol Policy) (*Relay, *upstream) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	for session, parent := range c.sessions {
		f.claims[session] = claim.Claim{ProjectID: "p1", Tool: "opencode", Parent: parent, Root: "/work/repo", Cwd: "/work/repo"}
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

// opencodeForwarded is what upstream received: every attribute key with each value it
// carried, and the bodies of the log records by event.
type opencodeForwarded struct {
	values map[string][]*commonpb.AnyValue
	bodies map[string][]string
	status []string
}

func (u *upstream) opencode(t *testing.T) opencodeForwarded {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := opencodeForwarded{values: map[string][]*commonpb.AnyValue{}, bodies: map[string][]string{}}
	note := func(attrs []*commonpb.KeyValue) {
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
					if ss.Scope.GetName() != opencodeScope {
						t.Errorf("a span left under scope %q", ss.Scope.GetName())
					}
					for _, sp := range ss.Spans {
						note(sp.Attributes)
						out.status = append(out.status, sp.Status.GetMessage())
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
						note(lr.Attributes)
						event := attr(lr.Attributes, "event.name")
						out.bodies[event] = append(out.bodies[event], lr.Body.GetStringValue())
					}
				}
			}
		}
	}
	return out
}

// The keys every case between them must carry through any policy, and the content each
// switch governs. A key a case did not send is not looked for in it.
var (
	opencodeSafe = []string{"session.id", "opencode.version", "opencode.parent_session.id", "opencode.parent_message.id",
		"opencode.message.id", "opencode.call.id", "opencode.call.kind", "opencode.agent", "opencode.completion.length",
		"opencode.prompt.length", "gen_ai.tool.call.id", "gen_ai.tool.name", "opencode.tool.status", "opencode.tool.denial",
		"opencode.tool.exit_code", "opencode.tool.output_bytes", "opencode.tool.subagent_session.id",
		"opencode.mcp.server", "opencode.mcp.tool", "gen_ai.request.model", "gen_ai.provider.name", "error.type",
		"gen_ai.usage.input_tokens", "gen_ai.usage.total_cost", "gen_ai.response.finish_reasons", "event.name"}
	opencodePromptKeys = []string{"gen_ai.completion"}
	opencodeToolKeys   = []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.files", "opencode.tool.title"}
	opencodeBothKeys   = []string{"opencode.error.message"}
	opencodeBodies     = []string{"opencode.user_prompt", "opencode.session.title"}
)

// Under each content policy, every case's keys are classified (none counted as no rule's),
// what the policy collects arrives, the content it withholds stays here, and the ids, counts
// and names that place and measure a call leave whatever it says. The error a provider
// returned goes when either switch is off; the files a tool touched are a list of strings,
// which follows the tool content switch alone.
func TestRelayAppliesEachPolicyToTheOpenCodeExport(t *testing.T) {
	t.Parallel()
	sentAnywhere := map[string]bool{}
	for _, name := range opencodeCases {
		maps.Copy(sentAnywhere, loadOpenCodeCase(t, name).sent)
	}
	for _, k := range slices.Concat(opencodeSafe, opencodePromptKeys, opencodeToolKeys, opencodeBothKeys) {
		if !sentAnywhere[k] {
			t.Errorf("no captured case sends %q: the test would prove nothing of it", k)
		}
	}
	for _, name := range opencodeCases {
		for _, pol := range []Policy{{}, {IncludePrompts: true}, {IncludeToolContent: true}, {IncludePrompts: true, IncludeToolContent: true}} {
			t.Run(name+policyName(pol), func(t *testing.T) {
				t.Parallel()
				c := loadOpenCodeCase(t, name)
				r, u := opencodeRelay(t, c, pol)
				for counter := range r.Stats().Snapshot().Counters {
					if strings.HasPrefix(counter, "unclassified.") {
						t.Errorf("the relay withheld a key no rule names: %s", counter)
					}
				}
				got := u.opencode(t)
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
				arrives(opencodeSafe, true)
				arrives(opencodePromptKeys, pol.IncludePrompts)
				arrives(opencodeToolKeys, pol.IncludeToolContent)
				arrives(opencodeBothKeys, pol.IncludePrompts && pol.IncludeToolContent)
				for _, event := range opencodeBodies {
					for _, body := range got.bodies[event] {
						if (body != "") != pol.IncludePrompts {
							t.Errorf("%s body %q under prompts %v", event, body, pol.IncludePrompts)
						}
					}
				}
				for _, files := range got.values["opencode.tool.files"] {
					list := files.GetArrayValue().GetValues()
					if len(list) == 0 || !strings.HasPrefix(list[0].GetStringValue(), "/home/user/work/") {
						t.Errorf("the files a tool touched arrived as %v", files)
					}
				}
				for _, m := range got.status {
					if m != "" {
						t.Errorf("a span carried a status message %q", m)
					}
				}
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

func policyName(p Policy) string {
	switch {
	case p.IncludePrompts && p.IncludeToolContent:
		return "/everything"
	case p.IncludePrompts:
		return "/tool-content-withheld"
	case p.IncludeToolContent:
		return "/prompts-withheld"
	}
	return "/both-withheld"
}

// A subagent's spans and records name the session that opened it, under any policy.
func TestRelayKeepsTheOpenCodeSubagentLink(t *testing.T) {
	t.Parallel()
	c := loadOpenCodeCase(t, "subagent")
	var child, parent string
	for s, p := range c.sessions {
		if p != "" {
			child, parent = s, p
		}
	}
	if child == "" {
		t.Fatal("the subagent case has no child session")
	}
	_, u := opencodeRelay(t, c, Policy{})
	got := u.opencode(t)
	var parents []string
	for _, v := range got.values["opencode.parent_session.id"] {
		parents = append(parents, v.GetStringValue())
	}
	if len(parents) == 0 || slices.ContainsFunc(parents, func(p string) bool { return p != parent }) {
		t.Errorf("the child's parent link arrived as %q, want %s", parents, parent)
	}
	var launched []string
	for _, v := range got.values["opencode.tool.subagent_session.id"] {
		launched = append(launched, v.GetStringValue())
	}
	if !slices.Contains(launched, child) {
		t.Errorf("the task tool's span names subagent %q, want %s", launched, child)
	}
}

// In global mode an OpenCode session's spans and records wait for the plugin's claim
// rather than going to the default project: the plugin claims every session before it
// exports any of it. Unclaimed, they are dropped, never sent.
func TestRelayHoldsAnUnclaimedOpenCodeSessionInGlobalMode(t *testing.T) {
	t.Parallel()
	c := loadOpenCodeCase(t, "subagent")
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
		if len(u.opencode(t).values) > 0 {
			t.Error("an unclaimed session's records left before its claim")
		}
		if claimed {
			f.mu.Lock()
			for session, parent := range c.sessions {
				f.claims[session] = claim.Claim{ProjectID: "p-default", Tool: "opencode", Parent: parent}
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
			if len(u.opencode(t).values) > 0 {
				t.Error("an unclaimed session's records left")
			}
		}
		srv.Close()
	}
}
