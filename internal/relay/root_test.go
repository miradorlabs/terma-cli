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

// stampsByKey is the resource attribute stamp each key's resources carried; a resource
// without one counts as "".
func (u *upstream) stampsByKey(t *testing.T, stamp string) map[string][]string {
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
			out[req.auth] = append(out[req.auth], attr(rl.Resource.Attributes, stamp))
		}
	}
	return out
}

// checkStamps asserts that every resource each key received carries want[stamp][key].
func (u *upstream) checkStamps(t *testing.T, want map[string]map[string]string) {
	t.Helper()
	for stamp, byKey := range want {
		got := u.stampsByKey(t, stamp)
		for key, w := range byKey {
			if len(got[key]) == 0 {
				t.Fatalf("nothing reached %s: %v", key, got)
			}
			for _, g := range got[key] {
				if g != w {
					t.Errorf("%s %s = %q, want %q", key, stamp, g, w)
				}
			}
		}
	}
}

// A claimed session's records carry the working tree its hooks ran in and the directory they
// ran in, Claude Code's (A) and Codex's (B) alike, unless the team's policy withholds tool
// content: a local path leaves only with the paths tool input names.
func TestRelayStampsTheClaimsWorkingTree(t *testing.T) {
	t.Parallel()
	for _, codexContent := range []bool{true, false} {
		u := newUpstream(t)
		f := newFixture()
		f.claim("A", claim.Claim{ProjectID: "p1", Tool: "claude-code", Root: "/work/a", Cwd: "/work/a/web"})
		f.claim("B", claim.Claim{ProjectID: "p2", Tool: "codex", Root: "/work/b", Cwd: "/work/b"})
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
		u.checkStamps(t, map[string]map[string]string{
			semconv.TermaRepositoryRootKey:   {"Bearer key-p1": "/work/a", "Bearer key-p2": wantB},
			semconv.TermaWorkingDirectoryKey: {"Bearer key-p1": "/work/a/web", "Bearer key-p2": wantB},
		})
	}
}

// Global mode places every part by its catch-all, which names no checkout: the root and the
// directory come from the session's own claim for that project, at the part's time (a late record
// from an earlier placement takes that one's), and a session no hook claimed has neither. B,
// Codex's, waits on its own claim, to a project with no key here.
func TestRelayStampsTheWorkingTreeUnderTheCatchAll(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	t0 := f.clock()
	f.claim("A", claim.Claim{ProjectID: "p-default", Tool: "claude-code", Root: "/work/now", Cwd: "/work/now/web", Placements: []claim.Placement{
		{ProjectID: "p-default", Root: "/work/before", Cwd: "/work/before", Since: t0.Add(-time.Hour)},
		{ProjectID: "p-default", Root: "/work/now", Cwd: "/work/now/web", Since: t0.Add(-time.Minute)},
	}})
	f.claim("B", claim.Claim{ProjectID: "p2", Root: "/elsewhere", Cwd: "/elsewhere"}) // another project's claim names neither here
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
	waitForCounters(t, r, func(c map[string]int) bool {
		return c["forwarded.logs"] == 4 && c["held_parts"] == 1
	})
	// A record from between the two placements, sent late, takes the earlier placement's root and directory.
	late := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "agent")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(t0.Add(-30 * time.Minute).UnixNano()),
			Attributes:   []*commonpb.KeyValue{kv("event.name", "user_prompt"), kv("session.id", "A")},
		}}}},
	}}}
	body, _ = proto.Marshal(late)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 5 })
	u.mu.Lock()
	defer u.mu.Unlock()
	seen := map[string]int{}
	for _, req := range u.requests {
		var m logspb.LogsData
		_ = proto.Unmarshal(req.body, &m)
		for _, rl := range m.ResourceLogs {
			stamps := attr(rl.Resource.Attributes, semconv.TermaRepositoryRootKey) + "|" + attr(rl.Resource.Attributes, semconv.TermaWorkingDirectoryKey)
			session := ""
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					session = attr(lr.Attributes, "session.id") + attr(lr.Attributes, "conversation.id")
				}
			}
			seen[session+"="+stamps]++
		}
	}
	if seen["A=/work/before|/work/before"] != 1 {
		t.Fatalf("A's late record does not carry its earlier placement's root and directory: %v", seen)
	}
	if seen["A=/work/now|/work/now/web"] == 0 {
		t.Fatalf("A's records do not carry its current root and directory: %v", seen)
	}
	for k := range seen {
		switch k {
		case "A=/work/now|/work/now/web", "A=/work/before|/work/before", "C=|", "D=|", "=|":
		default:
			t.Fatalf("unexpected session=root|directory %q in %v", k, seen)
		}
	}
}

// A part stamped while the policy collected tool content and sent after it stopped loses the
// root and the directory as content when it is withheld again at send time, never as
// unclassified keys.
func TestWithholdDropsTheRootWithToolContent(t *testing.T) {
	for _, flags := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		prompts, toolContent := flags[0], flags[1]
		res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "claude-code"),
			kv(semconv.TermaRepositoryRootKey, "/work/a"), kv(semconv.TermaWorkingDirectoryKey, "/work/a/web")}}
		p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("event.name", "api_request")}}}}}}}}}
		unclassified := map[string]int{}
		testRules.withhold(p, prompts, toolContent, unclassified)
		if got := attr(res.Attributes, semconv.TermaRepositoryRootKey); (got == "/work/a") != toolContent {
			t.Errorf("prompts=%v toolContent=%v: root is %q", prompts, toolContent, got)
		}
		if got := attr(res.Attributes, semconv.TermaWorkingDirectoryKey); (got == "/work/a/web") != toolContent {
			t.Errorf("prompts=%v toolContent=%v: directory is %q", prompts, toolContent, got)
		}
		if len(unclassified) != 0 {
			t.Errorf("prompts=%v toolContent=%v: unclassified %v", prompts, toolContent, unclassified)
		}
	}
}

// The working tree and the directory are the relay's to name: what the agent put on its own
// resource is dropped when no hook named them, and replaced when one did.
func TestRelayNamesTheWorkingTreeNotTheAgent(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture() // A claims neither
	f.claim("B", claim.Claim{ProjectID: "p2", Tool: "codex", Root: "/work/b", Cwd: "/work/b/cli"})
	policies := allPolicies(u)
	policies["p2"] = Policy{Endpoint: u.srv.URL, Key: "key-p2", IncludePrompts: true, IncludeToolContent: true}
	r, srv := f.relay(t, u, policies)
	logs := mixedLogs()
	res := logs.ResourceLogs[0].Resource
	res.Attributes = append(res.Attributes, kv(semconv.TermaRepositoryRootKey, "/agent/says"), kv(semconv.TermaWorkingDirectoryKey, "/agent/says"))
	body, _ := proto.Marshal(logs)
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	u.checkStamps(t, map[string]map[string]string{
		semconv.TermaRepositoryRootKey:   {"Bearer key-p1": "", "Bearer key-p2": "/work/b"},
		semconv.TermaWorkingDirectoryKey: {"Bearer key-p1": "", "Bearer key-p2": "/work/b/cli"},
	})
}
