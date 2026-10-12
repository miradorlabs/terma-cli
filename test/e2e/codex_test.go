package e2e

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
		t.Skip("live tests run only with TERMA_E2E=1")
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
		Record(t.Name(), "not run", "needs a ChatGPT login: TERMA_E2E_REAL_LOGIN=1 (copies ~/.codex/auth.json) or TERMA_E2E_CODEX_AUTH")
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
		} else if evs[0].Attrs["gen_ai.main_agent.name"] != "codex" {
			t.Errorf("session.start gen_ai.main_agent.name = %v", evs[0].Attrs["gen_ai.main_agent.name"])
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
			t.Errorf("exports not authorised with the live key: %v", auths)
		}

		// Production collection: the hook must deliver plan evidence itself.
		quota := sb.Delivered("terma.session.quota", tid, 45*time.Second)
		if len(quota) == 0 {
			t.Errorf("Codex Stop delivered no quota evidence for %s; %d Stop payloads; stderr: %s", tid, len(sb.HookPayloads("codex-stop")), tail(run.Stderr, 2000))
		} else {
			last := quota[len(quota)-1]
			checkCodexDeliveredQuota(t, last)
			CheckKeys(t, "codex/delivered-session-quota", last.Attrs, newest)
			Note("codex/plan_type", last.Attrs["terma.account.plan_type"])
		}
		// Delivery by the background flushes the Stop and SessionEnd hooks start.
		if d := sb.Delivered("terma.session.start", tid, 45*time.Second); len(d) == 0 {
			t.Errorf("terma.session.start never delivered for %s", tid)
		} else if d[0].Resource["mirador.project.id"] != sb.ProjectID || d[0].Attrs["gen_ai.main_agent.name"] != "codex" {
			t.Errorf("delivered session.start wrong: resource %v attrs %v", d[0].Resource, d[0].Attrs)
		}
		if len(sb.Delivered("terma.session.end", tid, 30*time.Second)) == 0 {
			t.Errorf("terma.session.end never delivered for %s", tid)
		}
		Note("codex/"+b.Label(), Version(b.Path)+" ("+string(route)+")")
	})
}

// TestCodexEditStampsCommit: a file the agent writes, with apply_patch or through the
// shell (#55), is attributed on the next commit.
func TestCodexEditStampsCommit(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		for _, tc := range []struct{ name, how string }{
			{"apply_patch", "Use apply_patch."},
			{"shell", "Do not use apply_patch: write it by running the shell command printf hello > hello.txt."},
		} {
			t.Run(tc.name, func(t *testing.T) { codexEditStampsCommit(t, b, newest, tc.how) })
		}
	})
}

func codexEditStampsCommit(t *testing.T, b Binary, newest bool, how string) {
	track(t)
	route := codexSubscription(t)
	sb := New(t, Isolated, WithCodex(b))
	run := sb.CodexExec(route,
		"Create a file named hello.txt in the current directory whose entire content is the word hello. "+how+" Then reply with exactly TERMA_OK.",
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
	} else if files := listAttr(evs[0].Attrs["terma.files.paths"]); !slices.Contains(files, "hello.txt") {
		t.Errorf("files touched = %q", files)
	}
	msg := sb.Commit("add hello")
	if !strings.Contains(msg, "Agent-Session-Id: "+tid) {
		t.Errorf("commit not stamped with the thread:\n%s", msg)
	}
	if !strings.Contains(msg, "Agent-Tool: codex") {
		t.Errorf("commit lacks Agent-Tool trailer:\n%s", msg)
	}
}

// TestCodexSameCallCommitIsStamped commits in the shell call that writes the file, as
// Codex often does: the call's PostToolUse comes after the commit, so PreToolUse stamps it.
func TestCodexSameCallCommitIsStamped(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		for _, tc := range []struct{ name, command string }{
			{"shell write", "printf hello > hello.txt && git add hello.txt && git commit -m hello"},
			{"patch from the shell", "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\nPATCH\ngit add hello.txt && git commit -m hello"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				track(t)
				route := codexSubscription(t)
				sb := New(t, Isolated, WithCodex(b))
				run := sb.CodexExec(route,
					"Run exactly this as ONE shell command, then reply with exactly TERMA_OK:\n"+tc.command,
					"-s", "workspace-write", "--add-dir", filepath.Join(sb.Repo, ".git"))
				if pre := sb.HookPayloads("codex-pre-tool-use"); len(pre) > 0 {
					CheckKeys(t, "codex/hook-PreToolUse", keysOf(pre[0]), newest)
				}
				msg := sb.git("log", "-1", "--format=%B")
				if !strings.Contains(msg, "hello") || !strings.Contains(msg, "Agent-Session-Id: "+run.ThreadID) {
					t.Fatalf("commit made in the same call not stamped with %s:\n%s\n%s", run.ThreadID, msg, tail(run.Stdout, 1500))
				}
			})
		}
	})
}

// TestCodexDeclinedWriteClaimsNothing: Codex runs PreToolUse before it asks to run a call,
// and the developer declines. The call never runs, so the developer's own commit of the file
// it named is not stamped. Offline: a fixture model, and an app-server client that declines.
func TestCodexDeclinedWriteClaimsNothing(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		var calls atomic.Int32
		provider := httptest.NewServer(codexWorkloadProvider(&calls, []string{"printf codex > NOTES.md"}))
		t.Cleanup(provider.Close)
		app := sb.StartAppServer(b, "terma-e2e", provider.URL)
		params := threadParams(sb.Repo, true)
		params["approvalPolicy"] = "untrusted" // a write asks first
		th, _ := app.call("thread/start", params)["thread"].(map[string]any)
		thread, _ := th["id"].(string)
		app.Turn(thread, "Do the task.")
		app.Close()
		if len(sb.HookPayloads("codex-pre-tool-use")) == 0 {
			t.Fatal("no PreToolUse: the scenario does not reach the claim it tests")
		}
		if _, err := os.Stat(filepath.Join(sb.Repo, "NOTES.md")); err == nil {
			t.Fatal("the declined call ran")
		}
		if err := os.WriteFile(filepath.Join(sb.Repo, "NOTES.md"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sb.git("add", "NOTES.md")
		sb.git("commit", "-m", "my notes")
		if msg := sb.git("log", "-1", "--format=%B"); strings.Contains(msg, "Agent-Session-Id") {
			t.Fatalf("the developer's commit was stamped by declined thread %s:\n%s", thread, msg)
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
			if a["terma.evidence.source"] != "codex_rollout" || quota[len(quota)-1].Resource["mirador.project.id"] != sb.ProjectID {
				t.Errorf("API quota source/project: %v", a)
			}
			// API routes may omit quota or explicitly report null. Neither is zero.
			switch a["terma.evidence.status"] {
			case "present", "unavailable", "not_ready":
			default:
				t.Errorf("API rollout could not be read: %v", a)
			}
		}
		CheckKeys(t, "codex/response_completed-apikey", completed[0].Attrs, newest)
	})
}
