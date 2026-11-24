package relay

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// testdata/grok_logs.json is the log export grok 1.0.92 sent for one headless prompt (OTLP/JSON,
// every content switch on, signed in, scrubbed), its six requests merged into one, with every
// content value replaced by CONTENT<key>: a subagent in a linked worktree that read, edited
// and ran git status, then the parent's shell call. The subagent's records carry its own
// session.id, in the parent's process; three records (an MCP connection, the subagent's launch
// and end) carry none.
const (
	grokParent   = "01a20a05-7443-7162-b2d7-43ca11701b1a"
	grokChild    = "01a20a05-a8e1-7b13-8d64-c57716a14568"
	grokSessions = 28
	grokHome     = "/home/dev"
)

func grokExport(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "grok_logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// grokRelay is a relay under pol with grok's session and its subagent's claimed, the
// subagent's claim naming its parent as grok-subagent-start writes it, and the export posted.
func grokRelay(t *testing.T, pol Policy) (*Relay, *upstream) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	f.claims[grokParent] = claim.Claim{ProjectID: "p1", Tool: "grok", Root: "/work/repo", Cwd: "/work/repo"}
	f.claims[grokChild] = claim.Claim{ProjectID: "p1", Tool: "grok", Parent: grokParent, Root: "/work/repo-child", Cwd: "/work/repo-child", Worktree: "subagent"}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, HomeDir: grokHome,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	if code := post(t, srv, "/v1/logs", grokExport(t), "application/json", token, false); code != http.StatusOK {
		t.Fatalf("the relay answered %d to grok's export", code)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == grokSessions })
	return r, u
}

// grokParts are the forwarded log parts: each one's resource and the sessions its records name.
func (u *upstream) grokParts(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*logspb.ResourceLogs
	for _, req := range u.requests {
		var m logspb.LogsData
		if req.path != "/v1/logs" {
			continue
		}
		if err := proto.Unmarshal(req.body, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.ResourceLogs...)
	}
	return out
}

func partSession(rl *logspb.ResourceLogs) string {
	for _, sl := range rl.ScopeLogs {
		for _, lr := range sl.LogRecords {
			if s := attr(lr.Attributes, "session.id"); s != "" {
				return s
			}
		}
	}
	return ""
}

// With content withheld, grok's prompts and replies are masked and its tool content stays
// here; its sessions, models, tokens, cost and tool outcomes leave, and the only key no rule
// names is user.email, which goes with them.
func TestRelayWithholdsGrokContent(t *testing.T) {
	t.Parallel()
	r, u := grokRelay(t, Policy{})
	sent := u.sent()
	if strings.Contains(sent, "CONTENT<") {
		t.Errorf("content left the machine: %q", sent[strings.Index(sent, "CONTENT<"):][:40])
	}
	if strings.Contains(sent, "dev@example.com") {
		t.Error("the developer's email left with content withheld")
	}
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") && name != "unclassified.user.email" {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
	for _, kept := range []string{grokParent, grokChild, "grok-4.7", "cost_usd_micros", "cache_read_tokens", "tool_use_id",
		"call-c1a10d56-5200-4741-813a-90d6c683fac3-1", "search_replace", "access_kind", "team.id", "grok_code.release_line", defaultMarker} {
		if !strings.Contains(sent, kept) {
			t.Errorf("the forwarded session lacks %q", kept)
		}
	}
}

// With prompts collected and tool content withheld, the prompt and the reply leave; a tool's
// parameters, input, output, command, error and file path do not.
func TestRelayWithholdsGrokToolContent(t *testing.T) {
	t.Parallel()
	_, u := grokRelay(t, Policy{IncludePrompts: true})
	sent := u.sent()
	for _, gone := range []string{"tool_parameters", "tool_input", "tool_output", "full_command", "error_message", "file_path"} {
		if strings.Contains(sent, "CONTENT<"+gone+">") {
			t.Errorf("tool content left the machine: %s", gone)
		}
	}
	for _, left := range []string{"CONTENT<prompt>", "CONTENT<response>"} {
		if !strings.Contains(sent, left) {
			t.Errorf("%s did not leave though prompts are collected", left)
		}
	}
}

// A subagent's records carry the session that opened it, from its claim; its parent's carry
// none. Every part of grok's names the relay's home directory with tool content, and none
// without: grok declares that it writes paths from `~` (HomeRelativeScopes).
func TestRelayStampsGrokParentAndHome(t *testing.T) {
	t.Parallel()
	for _, toolContent := range []bool{true, false} {
		_, u := grokRelay(t, Policy{IncludePrompts: true, IncludeToolContent: toolContent})
		seen := map[string]bool{}
		for _, rl := range u.grokParts(t) {
			session := partSession(rl)
			seen[session] = true
			want := map[string]string{grokChild: grokParent}[session]
			if got := attr(rl.Resource.Attributes, semconv.TermaSessionParentIDKey); got != want {
				t.Errorf("a part of %s carried parent %q, want %q", session, got, want)
			}
			home := ""
			if toolContent {
				home = grokHome
			}
			if got := attr(rl.Resource.Attributes, semconv.TermaHomeDirectoryKey); got != home {
				t.Errorf("tool content %v: a part of %s carried home %q, want %q", toolContent, session, got, home)
			}
		}
		if !seen[grokParent] || !seen[grokChild] {
			t.Errorf("parts by session: %v", seen)
		}
	}
}

// In global mode a grok session's records wait for their hook's claim rather than going to
// the default project: grok's hooks claim every session it runs, a subagent's too, and the
// claim carries the parent the relay stamps. Unclaimed, they are dropped, never sent.
func TestRelayHoldsAnUnclaimedGrokSessionInGlobalMode(t *testing.T) {
	t.Parallel()
	for _, claimed := range []bool{true, false} {
		u := newUpstream(t)
		f := newFixture()
		pol := Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
		r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
			CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
			Resolve:  func(claim.Claim) (Policy, error) { return pol, nil }})
		runRelay(t, r)
		srv := httptest.NewServer(r.Handler())
		post(t, srv, "/v1/logs", grokExport(t), "application/json", token, false)
		waitForCounters(t, r, func(c map[string]int) bool { return c["received.logs"] > 0 })
		if claimed {
			f.mu.Lock()
			f.claims[grokParent] = claim.Claim{ProjectID: "p-default", Tool: "grok"}
			f.claims[grokChild] = claim.Claim{ProjectID: "p-default", Tool: "grok", Parent: grokParent}
			f.mu.Unlock()
			waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] >= grokSessions })
		} else {
			f.mu.Lock()
			f.now = f.now.Add(DefaultTraceHold + time.Minute)
			f.mu.Unlock()
			waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unclaimed_expired.logs"] >= grokSessions })
		}
		for _, rl := range u.grokParts(t) {
			session := partSession(rl)
			if session == "" {
				continue // a record naming no session goes by process; the catch-all may take it
			}
			if !claimed {
				t.Errorf("an unclaimed session's records left: %s", session)
			}
			if attr(rl.Resource.Attributes, semconv.TermaRelayAttributionKey) != "" {
				t.Errorf("a part of %s was placed by inference", session)
			}
			if want := map[string]string{grokChild: grokParent}[session]; attr(rl.Resource.Attributes, semconv.TermaSessionParentIDKey) != want {
				t.Errorf("a part of %s lacks its parent", session)
			}
		}
		srv.Close()
	}
}

// dress is the one step a part goes through on its way out, and filterQueued the one that
// applies a team's policy again as a queued body leaves, from one team's outbox or a routed
// team's. Called directly here: dress stamps the home directory on a grok part with tool
// content under the policy it is given and on no other agent's part, and filterQueued takes
// it off again if the policy has tightened by the time the body is sent.
// TestRoutedGrokPartCarriesItsStamps drives the routed path itself.
func TestDressAndFilterQueuedCarryTheHomeStamp(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, HomeDir: grokHome})
	ran := claim.Claim{ProjectID: "p1", Tool: "grok", Root: "/work/repo", Cwd: "/work/repo"}
	open, withheld := Policy{IncludePrompts: true, IncludeToolContent: true}, Policy{IncludePrompts: true}
	logs := func(scope string) *part {
		return &part{signal: Logs, records: 1, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource:  &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: scope}, LogRecords: []*logspb.LogRecord{{}}}},
		}}}}
	}
	stamps := func(m *logspb.LogsData) (home, root string) {
		res := m.ResourceLogs[0].Resource.Attributes
		return attr(res, semconv.TermaHomeDirectoryKey), attr(res, semconv.TermaRepositoryRootKey)
	}
	const grokScope = "ai.xai.grok_code"

	p := logs(grokScope)
	if !r.dress(open, p, "p1", ran, attribution{}) {
		t.Fatal("the part was not dressed")
	}
	if home, root := stamps(p.msg.(*logspb.LogsData)); home != grokHome || root != "/work/repo" {
		t.Errorf("a grok part with tool content: home %q, root %q", home, root)
	}
	body, err := proto.Marshal(p.msg)
	if err != nil {
		t.Fatal(err)
	}
	for name, team := range map[string]*teamRoute{"one team's outbox": nil, "a routed team's outbox": {account: "a", enrollment: "e", tool: "grok"}} {
		out, _ := r.filterQueued(Logs, body, withheld, team)
		var sent logspb.LogsData
		if err := proto.Unmarshal(out, &sent); err != nil || len(sent.ResourceLogs) != 1 {
			t.Fatalf("%s: the queued body did not come back: %v", name, err)
		}
		if home, root := stamps(&sent); home != "" || root != "" {
			t.Errorf("%s, sent under a policy that now withholds tool content: home %q, root %q", name, home, root)
		}
	}

	p = logs(grokScope)
	r.dress(withheld, p, "p1", ran, attribution{})
	if home, root := stamps(p.msg.(*logspb.LogsData)); home != "" || root != "" {
		t.Errorf("a grok part with tool content withheld: home %q, root %q", home, root)
	}
	p = logs("some.other.agent")
	r.dress(open, p, "p1", ran, attribution{})
	if home, root := stamps(p.msg.(*logspb.LogsData)); home != "" || root != "/work/repo" {
		t.Errorf("another agent's part with tool content: home %q, root %q", home, root)
	}
}

// The routed path itself: a grok subagent's part, sent by a Router to a team's queue, leaves
// with the home directory, its own checkout and its parent under a team policy that collects
// tool content, and with its parent alone under one that withholds it.
func TestRoutedGrokPartCarriesItsStamps(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]bool{"tool content collected": true, "tool content withheld": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := newSignInUpstream(t)
			router := &stubRouter{
				active: true,
				routeFn: func(rec Record) Routed {
					return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_1", Project: "proj_1", Account: accountA, Kind: "team", Session: rec.Session, At: rec.At, EndAt: rec.EndAt}}
				},
				resolveFn: func(_, _, account string) (Policy, error) {
					return Policy{Endpoint: u.srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: content}, nil
				},
			}
			_, srv := startTeamRelay(t, Options{
				SignIn: &testSignIn{account: accountA}, Router: router, HomeDir: grokHome,
				Lookup: func(string, time.Time) (claim.Claim, bool) {
					return claim.Claim{ProjectID: "proj_1", Tool: "grok", Parent: grokParent, Root: "/work/repo-child", Cwd: "/work/repo-child", Worktree: "subagent"}, true
				},
			})
			logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{},
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope: &commonpb.InstrumentationScope{Name: "ai.xai.grok_code"},
					LogRecords: []*logspb.LogRecord{{
						TimeUnixNano: uint64(time.Now().UnixNano()),
						Attributes:   []*commonpb.KeyValue{kv("session.id", grokChild)},
					}},
				}},
			}}}
			if code := post(t, srv, "/v1/logs", mustMarshal(t, logs), "application/x-protobuf", token, false); code != http.StatusOK {
				t.Fatalf("the relay answered %d", code)
			}
			waitFor(t, func() bool { return len(u.requests()) == 1 })
			var got logspb.LogsData
			if err := proto.Unmarshal(u.bodies()[0], &got); err != nil || len(got.ResourceLogs) != 1 {
				t.Fatalf("the routed body: %v", err)
			}
			res := got.ResourceLogs[0].Resource.Attributes
			home, root, parent := attr(res, semconv.TermaHomeDirectoryKey), attr(res, semconv.TermaRepositoryRootKey), attr(res, semconv.TermaSessionParentIDKey)
			wantHome, wantRoot := "", ""
			if content {
				wantHome, wantRoot = grokHome, "/work/repo-child"
			}
			if home != wantHome || root != wantRoot || parent != grokParent {
				t.Errorf("home %q, root %q, parent %q; want %q, %q, %q", home, root, parent, wantHome, wantRoot, grokParent)
			}
			if project := attr(res, semconv.MiradorProjectIDKey); project != "proj_1" {
				t.Errorf("project %q", project)
			}
		})
	}
}
