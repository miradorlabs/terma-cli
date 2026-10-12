package e2e

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The assistant's reply, as Claude Code draws it: a ⏺ before the text. The
// typed prompt contains the same word, so the marker is what tells them apart.
var termaOK = regexp.MustCompile(`⏺\s*TERMA_OK`)

// Terma's mark at the start of the status line row, before the model name a
// default line shows or the marker the sandbox's own renderer prints.
var markRE = regexp.MustCompile(`(?m)^\s*t\s*(Haiku|LIVE-RENDERER)`)

// What the matrix promises for Claude Code, as the funding estimator and the
// session join read it. Every build under test must carry every one of these;
// the golden files record everything else a build happened to send.
var (
	requiredAPIRequest = []string{"session.id", "prompt.id", "model", "cost_usd", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_creation_tokens", "speed", "request_id"}
	requiredIdentity = []string{"organization.id", "user.account_uuid", "user.id"}
	requiredQuota    = []string{"terma.rate_limit.five_hour.used_percent", "terma.rate_limit.seven_day.used_percent",
		"terma.rate_limit.five_hour.resets_at", "terma.rate_limit.seven_day.resets_at", "gen_ai.request.model", "gen_ai.main_agent.name"}
)

// forEachClaude runs a scenario against every Claude Code build under test.
func forEachClaude(t *testing.T, run func(t *testing.T, b Binary, newest bool)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	builds := ClaudeBinaries(t)
	if len(builds) == 0 {
		t.Skip("no Claude Code build to test")
	}
	for i, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b, i == len(builds)-1 && !versionLess(b.Version, "2.1.272")) })
	}
}

// TestClaudeSubscriptionSession is the matrix's Claude Code row for a seat:
// hooks announce the session, the status line hands over the plan's windows,
// the exporter reports the call with the same session id, and the developer's
// own status line still draws.
func TestClaudeSubscriptionSession(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		mode, route := claudeMode(t)
		sb := New(t, mode, WithClaude(b))
		run := sb.ClaudeInteractive(route, "Reply with exactly TERMA_OK and nothing else.", termaOK, "--tools", "")
		sid := run.SessionID

		// Hooks: session start and end, from the committed project settings.
		if evs := sb.WaitEvents("terma.session.start", sid, 10*time.Second); len(evs) == 0 {
			t.Errorf("no terma.session.start for %s; spool: %+v", sid, sb.Spool())
		} else if evs[0].Attrs["gen_ai.main_agent.name"] != "claude-code" {
			t.Errorf("session.start gen_ai.main_agent.name = %v", evs[0].Attrs["gen_ai.main_agent.name"])
		}
		if evs := sb.WaitEvents("terma.session.end", sid, 15*time.Second); len(evs) == 0 {
			t.Errorf("no terma.session.end for %s", sid)
		}

		// Status line: the plan's windows, after the first response. Live-verified
		// on a Team seat on 2026-09-15; this is that observation as a test.
		quota := sb.WaitEvents("terma.session.quota", sid, 5*time.Second)
		if len(quota) == 0 {
			t.Errorf("no terma.session.quota for %s: the status line did not run or carried no rate_limits", sid)
		} else {
			last := quota[len(quota)-1]
			for _, k := range requiredQuota {
				if _, ok := last.Attrs[k]; !ok {
					t.Errorf("quota event lacks %s: %+v", k, last.Attrs)
				}
			}
			CheckKeys(t, "claude/session-quota", keysOf(last.Attrs), newest)
		}

		// The terminal: Terma's mark in front of the status line (Claude re-renders
		// the text, so the check is on what a person sees), and in isolated mode the
		// wrapped renderer's own output behind it.
		if !markRE.MatchString(run.Text) {
			t.Errorf("status line shows no terma mark; last screen:\n%s", tail(run.Text, 1500))
		}
		if mode == Isolated {
			if !strings.Contains(run.Text, sb.RendererMarker) {
				t.Errorf("the pre-existing status line (%s) no longer draws:\n%s", sb.RendererMarker, tail(run.Text, 1500))
			}
		} else {
			Note("claude/statusline-passthrough", "not checked in real-login mode (record lives under the scratch config)")
		}

		// The exporter: the call, joined to the hook session by session.id, carrying
		// what the funding estimator reads, authorised with the live key.
		reqs := sb.APIRequests(sid, 30*time.Second)
		if len(reqs) == 0 {
			t.Fatalf("no api_request reached the receiver for %s; %d logs total", sid, len(sb.Receiver.Logs()))
		}
		first := reqs[0]
		checkClaudeRequests(t, reqs)
		if len(quota) > 0 {
			checkClaudeQuota(t, quota[len(quota)-1], reqs)
		}
		for _, k := range requiredAPIRequest {
			if _, ok := first.Attrs[k]; !ok {
				t.Errorf("api_request lacks %s: %v", k, first.Attrs)
			}
		}
		// Identity rides on the record, stamped from the stored login.
		for _, k := range requiredIdentity {
			if _, ok := first.Attrs[k]; !ok {
				if _, ok := first.Resource[k]; !ok {
					t.Errorf("no %s on a subscription login", k)
				}
			}
		}
		CheckKeys(t, "claude/api_request", first.Attrs, newest)
		CheckKeys(t, "claude/resource", first.Resource, newest)
		if auths := sb.Receiver.Authorizations(); len(auths) == 0 || auths[0] != "Bearer "+liveKey {
			t.Errorf("exports not authorised with the live key: %v", auths)
		}
		AddSpend(SpendOf(reqs))
		checkClaudeAccount(t, sb, sid, mode == RealLogin)

		// Delivery: the Stop and SessionEnd hooks start background flushes, so the
		// spooled events must reach the receiver on their own, as OTLP logs from
		// terma-cli, stamped with the project, under the live key. This is
		// the shape the backend parses.
		delivered := sb.Delivered("terma.session.quota", sid, 45*time.Second)
		if len(delivered) == 0 {
			t.Errorf("no terma.session.quota delivered to the receiver for %s (spool has %d)", sid, len(sb.Events("terma.session.quota", sid)))
		} else {
			d := delivered[len(delivered)-1]
			attrs := make(map[string]any, len(d.Attrs))
			for k, v := range d.Attrs {
				attrs[k] = v
			}
			checkClaudeQuota(t, Event{Time: d.Time, SessionID: d.Attrs["session.id"], Attrs: attrs}, reqs)
			for _, k := range requiredQuota {
				if _, ok := d.Attrs[k]; !ok {
					t.Errorf("delivered quota event lacks %s: %v", k, d.Attrs)
				}
			}
			// The project leaves on the resource alone: the spool's routing key stays behind.
			if d.Resource["mirador.project.id"] != sb.ProjectID || d.Resource["terma.schema.version"] != "2" || d.Attrs["project_id"] != "" {
				t.Errorf("delivered event not stamped with the project: resource %v attrs %v", d.Resource, d.Attrs)
			}
			CheckKeys(t, "claude/delivered-session-quota", d.Attrs, newest)
		}
		if len(sb.Delivered("terma.session.start", sid, 30*time.Second)) == 0 {
			t.Errorf("terma.session.start never delivered for %s", sid)
		}
		if len(sb.Delivered("terma.session.end", sid, 30*time.Second)) == 0 {
			t.Errorf("terma.session.end never delivered for %s", sid)
		}
		Note("claude/"+b.Label(), Version(b.Path)+" ("+string(route)+", "+modeName(mode)+")")
	})
}

// TestClaudeEditStampsCommit: an edit the agent makes is attributed on the next
// commit through the committed git hooks.
func TestClaudeEditStampsCommit(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		mode, route := claudeMode(t)
		sb := New(t, mode, WithClaude(b))
		run := sb.ClaudeInteractive(route,
			"Use the Write tool to create a file named hello.txt whose entire content is the word hello. Then reply with exactly TERMA_OK.",
			termaOK, "--allowedTools", "Write", "--tools", "Write", "--permission-mode", "acceptEdits")
		sid := run.SessionID
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v\n%s", sid, sb.Spool(), tail(run.Text, 2000))
		} else if files := listAttr(evs[0].Attrs["terma.files.paths"]); !slices.Contains(files, "hello.txt") {
			t.Errorf("files touched = %q", files)
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) {
			t.Errorf("commit not stamped with the session:\n%s", msg)
		}
		if !strings.Contains(msg, "Agent-Tool: claude-code") {
			t.Errorf("commit lacks Agent-Tool trailer:\n%s", msg)
		}
		if evs := sb.WaitEvents("terma.commit.stamped", sid, 10*time.Second); len(evs) == 0 {
			t.Errorf("no terma.commit.stamped for %s", sid)
		}
		AddSpend(SpendOf(sb.APIRequests(sid, 20*time.Second)))
	})
}

// TestClaudeAPIKeyHeadless is the Console route: the call is exported and the
// session announced, and the status line, which does not run headless, sends
// nothing. Needs ANTHROPIC_API_KEY.
func TestClaudeAPIKeyHeadless(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		if ClaudeCredentials().APIKey == "" {
			Record(t.Name(), "not run", "needs ANTHROPIC_API_KEY")
			t.Skip("no ANTHROPIC_API_KEY")
		}
		sb := New(t, Isolated, WithClaude(b))
		result, sid := sb.ClaudeHeadless(RouteAPIKey, "Reply with exactly TERMA_OK and nothing else.", "--tools", "")
		if got, _ := result["session_id"].(string); got != sid {
			t.Errorf("session_id %q, asked for %q", got, sid)
		}
		if evs := sb.WaitEvents("terma.session.start", sid, 10*time.Second); len(evs) == 0 {
			t.Errorf("no terma.session.start for headless %s", sid)
		}
		reqs := sb.APIRequests(sid, 30*time.Second)
		if len(reqs) == 0 {
			t.Fatalf("no api_request reached the receiver for %s", sid)
		}
		checkClaudeRequests(t, reqs)
		for _, k := range requiredAPIRequest {
			if _, ok := reqs[0].Attrs[k]; !ok {
				t.Errorf("api_request lacks %s: %v", k, reqs[0].Attrs)
			}
		}
		checkClaudeAccount(t, sb, sid, false)
		if quota := sb.Events("terma.session.quota", sid); len(quota) > 0 {
			Note("claude/headless", "status line ran headless and produced a quota event")
		}
		if cost, ok := result["total_cost_usd"].(float64); ok {
			AddSpend(cost)
		}
		CheckKeys(t, "claude/api_request-apikey", reqs[0].Attrs, newest)
		CheckKeys(t, "claude/resource-apikey", reqs[0].Resource, newest)
	})
}

func modeName(m Mode) string {
	if m == Isolated {
		return "isolated"
	}
	return "real-login"
}

// TestClaudeEnvRoutes pins which credential each isolated route hands Claude Code.
func TestClaudeEnvRoutes(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "dummy-api-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "subscription-token")
	for _, tc := range []struct {
		name      string
		route     Route
		want, not string
	}{
		{"subscription", RouteSubscription, "CLAUDE_CODE_OAUTH_TOKEN=subscription-token", "ANTHROPIC_API_KEY="},
		{"api key", RouteAPIKey, "ANTHROPIC_API_KEY=dummy-api-key", "CLAUDE_CODE_OAUTH_TOKEN="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &Sandbox{T: t, Mode: Isolated, Dir: t.TempDir(), Terma: "/tmp/terma"}
			env := sb.claudeEnv(tc.route)
			if !slices.Contains(env, tc.want) {
				t.Errorf("env lacks %s", tc.want)
			}
			if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, tc.not) }) {
				t.Errorf("env carries %s on the %s route", tc.not, tc.name)
			}
		})
	}
}
