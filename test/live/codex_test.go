package live

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// What the matrix promises for Codex: the route stated per event, the call
// joined to the hook session by conversation id, and the plan state in the
// rollout that the Stop hook reads. Missing edit hooks must never count as a
// passing attribution test: incomplete installations can suppress tool use.
var (
	requiredCodexCompleted = []string{"conversation.id", "auth_mode", "model", "app.version",
		"input_token_count", "output_token_count", "cached_token_count"}
)

func forEachCodex(t *testing.T, run func(t *testing.T, b Binary, newest bool)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	builds := CodexBinaries(t)
	if len(builds) == 0 {
		t.Skip("no Codex build to test")
	}
	for i, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b, i == len(builds)-1) })
	}
}

// summarize renders hook payloads with long strings cut, for a failure message.
func summarize(payloads []map[string]any) string {
	var b strings.Builder
	for i, p := range payloads {
		if i >= 3 {
			b.WriteString(" …")
			break
		}
		b.WriteString("\n  ")
		for k, v := range p {
			s := strings.ReplaceAll(strings.TrimSpace(fmtAny(v)), "\n", "⏎")
			if len(s) > 160 {
				s = s[:160] + "…"
			}
			b.WriteString(k + "=" + s + " ")
		}
	}
	return b.String()
}

func codexSubscription(t *testing.T) Route {
	t.Helper()
	if CodexCredentials().AuthFile == "" {
		Record(t.Name(), "not run", "needs a ChatGPT login: TERMA_LIVE_REAL_LOGIN=1 (copies ~/.codex/auth.json) or TERMA_LIVE_CODEX_AUTH")
		t.Skip("no Codex login")
	}
	return RouteSubscription
}

// TestCodexSubscriptionSession is the matrix's Codex row for a ChatGPT login.
func TestCodexSubscriptionSession(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		route := codexSubscription(t)
		sb := New(t, Isolated, WithCodex(b))
		run := sb.CodexExec(route, "Reply with exactly TERMA_OK and nothing else. Do not use any tools.", "-s", "read-only")
		tid := run.ThreadID
		if !strings.Contains(run.Stdout, "TERMA_OK") {
			t.Errorf("no TERMA_OK in codex output:\n%s", tail(run.Stdout, 2000))
		}

		// Hooks from the committed .codex/hooks.json, run under the bypass flag.
		if evs := sb.WaitEvents("terma.session.start", tid, 10*time.Second); len(evs) == 0 {
			t.Errorf("no terma.session.start for thread %s; spool: %+v\ncodex stderr:\n%s", tid, sb.Spool(), tail(run.Stderr, 2000))
		} else if evs[0].Attrs["tool"] != "codex" {
			t.Errorf("session.start tool = %v", evs[0].Attrs["tool"])
		}
		if evs := sb.WaitEvents("terma.session.end", tid, 15*time.Second); len(evs) == 0 {
			t.Errorf("no terma.session.end for thread %s", tid)
		}

		// The exporter: conversation start and the completed response, both
		// naming the credential class.
		starts, completed := sb.CodexLogs(tid, 30*time.Second)
		if len(completed) == 0 {
			t.Fatalf("no response.completed reached the receiver for %s; %d logs total", tid, len(sb.Receiver.Logs()))
		}
		if len(starts) == 0 {
			t.Errorf("no codex.conversation_starts for %s", tid)
		}
		first := completed[0]
		checkCodexCompleted(t, completed)
		for _, r := range completed {
			Note("codex/usage-record", "input="+r.Attrs["input_token_count"]+" output="+r.Attrs["output_token_count"]+" cached="+r.Attrs["cached_token_count"])
		}
		for _, k := range requiredCodexCompleted {
			if _, ok := first.Attrs[k]; !ok {
				t.Errorf("response.completed lacks %s: %v", k, first.Attrs)
			}
		}
		// The docs say "swic"; 0.154.0 emits "Chatgpt". Both mean the ChatGPT login,
		// and the estimator has to accept both spellings.
		if am := strings.ToLower(first.Attrs["auth_mode"]); am != "swic" && am != "chatgpt" {
			t.Errorf("auth_mode = %q on a ChatGPT login", first.Attrs["auth_mode"])
		}
		Note("codex/auth_mode", "ChatGPT login reports auth_mode="+first.Attrs["auth_mode"])
		CheckKeys(t, "codex/response_completed", first.Attrs, newest)
		CheckKeys(t, "codex/resource", first.Resource, newest)
		if auths := sb.Receiver.Authorizations(); len(auths) == 0 || auths[0] != "Bearer "+liveKey {
			t.Errorf("exports not authorised with the connected key: %v", auths)
		}

		// Production collection: the hook must deliver plan evidence itself.
		quota := sb.Delivered("terma.session.quota", tid, 45*time.Second)
		if len(quota) == 0 {
			t.Errorf("Codex Stop delivered no quota evidence for %s; %d Stop payloads; stderr: %s", tid, len(sb.HookPayloads("codex-stop")), tail(run.Stderr, 2000))
		} else {
			last := quota[len(quota)-1]
			checkCodexDeliveredQuota(t, last)
			CheckKeys(t, "codex/delivered-session-quota", last.Attrs, newest)
			Note("codex/plan_type", last.Attrs["plan_type"])
		}
		// Delivery by the background flushes the Stop and SessionEnd hooks start.
		if d := sb.Delivered("terma.session.start", tid, 45*time.Second); len(d) == 0 {
			t.Errorf("terma.session.start never delivered for %s", tid)
		} else if d[0].Resource["mirador.project.id"] != sb.ProjectID || d[0].Attrs["tool"] != "codex" {
			t.Errorf("delivered session.start wrong: resource %v attrs %v", d[0].Resource, d[0].Attrs)
		}
		if len(sb.Delivered("terma.session.end", tid, 30*time.Second)) == 0 {
			t.Errorf("terma.session.end never delivered for %s", tid)
		}
		Note("codex/"+b.Label(), Version(b.Path)+" ("+string(route)+")")
	})
}

// TestCodexEditStampsCommit: an apply_patch the agent makes is attributed on
// the next commit.
func TestCodexEditStampsCommit(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		route := codexSubscription(t)
		sb := New(t, Isolated, WithCodex(b))
		run := sb.CodexExec(route,
			"Create a file named hello.txt in the current directory whose entire content is the word hello. Use apply_patch. Then reply with exactly TERMA_OK.",
			"-s", "workspace-write")
		tid := run.ThreadID
		// What this build handed the PostToolUse hook is evidence whether or not
		// terma understood it.
		post := sb.HookPayloads("codex-post-tool-use")
		if len(post) > 0 {
			CheckKeys(t, "codex/hook-PostToolUse", keysOf(post[0]), newest)
			if ti, ok := post[0]["tool_input"].(map[string]any); ok {
				CheckKeys(t, "codex/hook-PostToolUse-tool_input", keysOf(ti), newest)
			}
		}
		if evs := sb.WaitEvents("terma.files.touched", tid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; %d PostToolUse payload(s): %s\nspool: %+v\n%s",
				tid, len(post), summarize(post), sb.Spool(), tail(run.Stdout, 1500))
		} else if files, _ := evs[0].Attrs["files"].(string); !strings.Contains(files, "hello.txt") {
			t.Errorf("files touched = %q", files)
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+tid) {
			t.Errorf("commit not stamped with the thread:\n%s", msg)
		}
		if !strings.Contains(msg, "Agent-Tool: codex") {
			t.Errorf("commit lacks Agent-Tool trailer:\n%s", msg)
		}
	})
}

// TestCodexAPIKey is the API route: auth_mode says so. Needs OPENAI_API_KEY.
func TestCodexAPIKey(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		track(t)
		if CodexCredentials().APIKey == "" {
			Record(t.Name(), "not run", "needs OPENAI_API_KEY")
			t.Skip("no OPENAI_API_KEY")
		}
		sb := New(t, Isolated, WithCodex(b))
		run := sb.CodexExec(RouteAPIKey, "Reply with exactly TERMA_OK and nothing else. Do not use any tools.", "-s", "read-only")
		_, completed := sb.CodexLogs(run.ThreadID, 30*time.Second)
		if len(completed) == 0 {
			t.Fatalf("no response.completed for %s", run.ThreadID)
		}
		checkCodexCompleted(t, completed)
		for _, r := range completed {
			Note("codex/usage-record", "input="+r.Attrs["input_token_count"]+" output="+r.Attrs["output_token_count"]+" cached="+r.Attrs["cached_token_count"])
		}
		if am := strings.ToLower(completed[0].Attrs["auth_mode"]); am != "api" && am != "apikey" {
			t.Errorf("auth_mode = %q on an API key", completed[0].Attrs["auth_mode"])
		}
		Note("codex/auth_mode", "API key reports auth_mode="+completed[0].Attrs["auth_mode"])
		quota := sb.Delivered("terma.session.quota", run.ThreadID, 30*time.Second)
		if len(quota) == 0 {
			t.Error("API route did not deliver its quota availability status")
		} else {
			a := quota[len(quota)-1].Attrs
			if a["evidence_source"] != "codex_rollout" || a["project_id"] != sb.ProjectID {
				t.Errorf("API quota source/project: %v", a)
			}
			// API routes may omit quota or explicitly report null. Neither is zero.
			switch a["evidence_status"] {
			case "present", "unavailable", "not_ready":
			default:
				t.Errorf("API rollout could not be read: %v", a)
			}
		}
		CheckKeys(t, "codex/response_completed-apikey", completed[0].Attrs, newest)
	})
}
