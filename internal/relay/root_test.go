package relay

import (
	"net/http/httptest"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// rootsByKey is the terma.repository.root each key's resources carried; a resource without
// one counts as "".
func (u *upstream) rootsByKey(t *testing.T) map[string][]string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string][]string{}
	for _, req := range u.requests {
		if req.path != "/v1/logs" {
			continue
		}
		var m logspb.LogsData
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.ResourceLogs {
			out[req.auth] = append(out[req.auth], attr(rl.Resource.Attributes, semconv.TermaRepositoryRootKey))
		}
	}
	return out
}

// A claimed session's records carry the working tree its hooks ran in, Claude Code's (A) and
// Codex's (B) alike, unless the team's policy withholds tool content: a local path leaves
// only with the paths tool input names.
func TestRelayStampsTheClaimsWorkingTree(t *testing.T) {
	t.Parallel()
	for _, codexContent := range []bool{true, false} {
		u := newUpstream(t)
		f := newFixture()
		f.claim("A", claim.Claim{ProjectID: "p1", Tool: "claude-code", Root: "/work/a"})
		f.claim("B", claim.Claim{ProjectID: "p2", Tool: "codex", Root: "/work/b"})
		policies := allPolicies(u)
		policies["p2"] = Policy{Endpoint: u.srv.URL, Key: "key-p2", IncludePrompts: codexContent, IncludeToolContent: codexContent}
		r, srv := f.relay(t, u, policies)
		body, _ := proto.Marshal(mixedLogs())
		post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
		waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
		wantB := ""
		if codexContent {
			wantB = "/work/b"
		}
		roots := u.rootsByKey(t)
		for key, want := range map[string]string{"Bearer key-p1": "/work/a", "Bearer key-p2": wantB} {
			if len(roots[key]) == 0 {
				t.Fatalf("codex content %v: nothing reached %s: %v", codexContent, key, roots)
			}
			for _, got := range roots[key] {
				if got != want {
					t.Errorf("codex content %v: %s root = %q, want %q", codexContent, key, got, want)
				}
			}
		}
	}
}

// Global mode places every part by its catch-all, which names no checkout: the root comes
// from the session's own claim for that project, at the part's time, and a session no hook
// claimed has none.
func TestRelayStampsTheWorkingTreeUnderTheCatchAll(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	t0 := f.clock()
	f.claim("A", claim.Claim{ProjectID: "p-default", Tool: "claude-code", Root: "/work/now", Placements: []claim.Placement{
		{ProjectID: "p-default", Root: "/work/before", Since: t0.Add(-time.Hour)},
		{ProjectID: "p-default", Root: "/work/now", Since: t0.Add(-time.Minute)},
	}})
	f.claim("B", claim.Claim{ProjectID: "p2", Root: "/elsewhere"}) // another project's claim names no root here
	policies := map[string]Policy{"p-default": {Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := policies[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 6 })
	u.mu.Lock()
	defer u.mu.Unlock()
	seen := map[string]int{}
	for _, req := range u.requests {
		var m logspb.LogsData
		_ = proto.Unmarshal(req.body, &m)
		for _, rl := range m.ResourceLogs {
			root := attr(rl.Resource.Attributes, semconv.TermaRepositoryRootKey)
			session := ""
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					session = attr(lr.Attributes, "session.id") + attr(lr.Attributes, "conversation.id")
				}
			}
			seen[session+"="+root]++
		}
	}
	if seen["A=/work/now"] == 0 {
		t.Fatalf("A's records do not carry its current root: %v", seen)
	}
	for k := range seen {
		switch k {
		case "A=/work/now", "B=", "C=", "D=", "=":
		default:
			t.Fatalf("unexpected session=root %q in %v", k, seen)
		}
	}
}

// A part stamped while the policy collected tool content and sent after it stopped loses the
// root as content when it is withheld again at send time, never as an unclassified key.
func TestWithholdDropsTheRootWithToolContent(t *testing.T) {
	for _, flags := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		prompts, toolContent := flags[0], flags[1]
		res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "claude-code"), kv(semconv.TermaRepositoryRootKey, "/work/a")}}
		p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("event.name", "api_request")}}}}}}}}}
		unclassified := map[string]int{}
		testRules.withhold(p, prompts, toolContent, unclassified)
		if got := attr(res.Attributes, semconv.TermaRepositoryRootKey); (got == "/work/a") != toolContent {
			t.Errorf("prompts=%v toolContent=%v: root is %q", prompts, toolContent, got)
		}
		if len(unclassified) != 0 {
			t.Errorf("prompts=%v toolContent=%v: unclassified %v", prompts, toolContent, unclassified)
		}
	}
}
