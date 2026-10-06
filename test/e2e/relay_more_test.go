package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Codex resumed somewhere else: `codex exec resume <id>` from a personal directory.
// Whatever Codex does with the id, nothing the resumed run does may reach upstream.
func TestRelayCodexResumedElsewhere(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.resumed_elsewhere")
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		first := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		calls.Store(1) // the resumed turn gets a plain reply, no tool call
		sb.WorkDir = personal
		resumed := sb.CodexExec(RouteAPIKey, "TERMA_PERSONAL_WORK please", append(fixtureCodexArgs(provider.URL), "resume", first.ThreadID)...)
		time.Sleep(8 * time.Second)
		e := sb.Receiver.evidence()
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		prompts := 0
		for _, r := range e.logs {
			if r.Attrs["event.name"] == "codex.user_prompt" && strings.Contains(r.Attrs["prompt"], "TERMA_PERSONAL_WORK") {
				prompts++
			}
		}
		if prompts > 0 || len(leakedFieldsOf(e, "TERMA_PERSONAL_WORK")) > 0 {
			t.Errorf("the resumed personal run reached upstream (thread %s → %s): %v", first.ThreadID, resumed.ThreadID, leakedFieldsOf(e, "TERMA_PERSONAL_WORK"))
		}
		Note(t.Name(), "resumed thread id "+map[bool]string{true: "kept", false: "changed"}[resumed.ThreadID == first.ThreadID])
	})
}

// A session in a linked worktree (git worktree add) of an admitted repository is admitted
// through its main repository's origin, so its telemetry must reach the team's project.
func TestRelayLinkedWorktree(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true})
		wt := filepath.Join(sb.Dir, "wt")
		sb.git("worktree", "add", "-q", wt)
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadlessIn(wt, RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		deadline := time.Now().Add(30 * time.Second)
		for len(reached(sb.Receiver)[sid]) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		got := reached(sb.Receiver)[sid]
		if !got["project "+sb.ProjectID] || !got["key Bearer "+liveKey] || len(got) != 2 {
			t.Errorf("a linked worktree's session reached upstream as %v", got)
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
	})
}

// A Claude subagent (the Agent tool) runs inside the parent's session and process:
// its requests and tools are the parent's session's, and are forwarded with it.
func TestRelayClaudeSubagent(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.subagents")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		steps := []map[string]any{{"name": "Agent", "input": map[string]any{"description": "check", "prompt": "Reply exactly TERMA_SUBAGENT_REPLY.", "subagent_type": "general-purpose"}}}
		var calls atomic.Int32
		provider := httptest.NewServer(claudeScriptedProvider(&calls, steps))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, "Use a subagent.", "--max-turns", "4", "--tools", "Agent", "--allowedTools", "Agent")
		if calls.Load() < 3 {
			t.Fatalf("provider calls = %d: no subagent ran", calls.Load())
		}
		deadline := time.Now().Add(30 * time.Second)
		for len(sb.APIRequests(sid, time.Second)) < 3 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		e := sb.Receiver.evidence()
		if n := len(sb.APIRequests(sid, time.Second)); n < 3 {
			t.Errorf("want the parent's and the subagent's api_request records forwarded, got %d", n)
		}
		var failures contractFailures
		checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
		for _, f := range failures {
			t.Error(f)
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "dropped.") != 0 {
			t.Errorf("a subagent's records were dropped: %v", c)
		}
	})
}

// A developer whose Codex does not trust terma's machine-wide hooks (setup approves them,
// so here the approvals are gone): Codex runs no hook, so nothing claims the session and
// nothing is forwarded — fail closed, and the reason is what doctor's "agent hooks run"
// check names.
func TestRelayCodexUntrustedHooks(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.CodexHooksUntrusted = true
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		if sb.codexTrustWithdrawn == 0 {
			t.Fatal("setup approved none of terma's Codex hooks, so there was nothing to withdraw")
		}
		time.Sleep(8 * time.Second)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("%d records forwarded for a session no hook claimed", n)
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "received.") == 0 || sum(c, "forwarded.") != 0 {
			t.Errorf("want received and nothing forwarded: %v", c)
		}
	})
}

// A Codex turn that outlasts the ordinary hold: the provider stalls mid-turn, so the
// turn's first child spans are exported well before the turn span that names the
// session. They wait under their trace — learnt from Codex's mid-turn logs, or from the
// turn span at the end — and the full contract still arrives.
func TestRelayCodexLongTurn(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		// The ordinary hold covers the claim racing Codex's first log (the hook claims
		// the session a few seconds after conversation_starts on most builds); the stall
		// outlasts it, so the turn's first child spans precede the turn span by more than
		// the ordinary hold and only the trace hold keeps them.
		const hold = 30 * time.Second
		sb.UseRelay(RelayOptions{Start: true, Hold: hold, Content: true})
		var calls atomic.Int32
		inner := codexTelemetryProvider(t, &calls)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Load() >= 1 {
				time.Sleep(hold + 5*time.Second) // the second request: past the hold
			}
			inner.ServeHTTP(w, r)
		}))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
			checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, false, true)
			checkOnlyClaimed(r, e, "conversation.id", run.ThreadID, sb.ProjectID)
		})
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
	})
}

// An interactive Claude session (the way developers use it) whose relay dies between
// turns. The next turn's UserPromptSubmit hook starts a new one before the turn
// exports anything, so no turn is lost; before that hook was wired, the whole turn
// after the crash was (its Stop hook came too late, and Claude's exporter does not
// retry a refused connection).
func TestRelayClaudeInteractiveRestart(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		const key = "synthetic-telemetry-key"
		t.Setenv("ANTHROPIC_API_KEY", key)
		sb := New(t, Isolated, WithClaude(b))
		// The key is approved already, as it is for a developer who has used it.
		state := map[string]any{}
		raw, _ := os.ReadFile(filepath.Join(sb.ClaudeConfig, ".claude.json"))
		_ = json.Unmarshal(raw, &state)
		state["customApiKeyResponses"] = map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}}
		raw, _ = json.MarshalIndent(state, "", "  ")
		sb.writeAbs(filepath.Join(sb.ClaudeConfig, ".claude.json"), string(raw))
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeScriptedProvider(&calls, nil))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		reply := regexp.MustCompile(`TERMA_TELEMETRY_REPLY`)
		prompts := []string{"turn one", "turn two", "turn three"}
		var restarted bool
		run := sb.ClaudeInteractiveTurns(RouteAPIKey, prompts, []*regexp.Regexp{reply, reply, reply}, func(turn int, sid string) {
			if turn == 0 {
				sb.StopRelay() // the relay dies; the next turn's hooks must start another
			}
			if turn == 1 {
				restarted = sb.waitRelay(10 * time.Second)
			}
		})
		deadline := time.Now().Add(30 * time.Second)
		perTurn := func() []int {
			counts := make([]int, len(prompts))
			for _, r := range sb.Receiver.evidence().logs {
				if r.Attrs["event.name"] != "user_prompt" || r.Attrs["session.id"] != run.SessionID {
					continue
				}
				for i, p := range prompts {
					if strings.Contains(r.Attrs["prompt"], p) {
						counts[i]++
					}
				}
			}
			return counts
		}
		for time.Now().Before(deadline) {
			if c := perTurn(); c[0] > 0 && c[2] > 0 {
				break
			}
			time.Sleep(time.Second)
		}
		counts := perTurn()
		var failures contractFailures
		checkOnlyClaimed(&failures, sb.Receiver.evidence(), "session.id", run.SessionID, sb.ProjectID)
		for _, f := range failures {
			t.Error(f)
		}
		if !restarted {
			t.Errorf("no hook restarted the relay after it died")
		}
		// The turn after the crash too: its UserPromptSubmit hook restarts the relay
		// before the turn exports anything.
		if counts[0] == 0 || counts[1] == 0 || counts[2] == 0 {
			t.Errorf("prompt records per turn %v: every turn must arrive, the one after the crash included", counts)
		}
		sb.StopRelay()
		Note(t.Name(), fmt.Sprintf("interactive restart: user_prompt records per turn %v (the relay died after turn 1)", counts))
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
	})
}

// codexTUI runs Codex's interactive TUI in a pty for one prompt, against provider,
// and returns once the reply is on screen and the session has had time to title
// itself and export.
func (sb *Sandbox) codexTUI(b Binary, providerURL, prompt string) {
	t := sb.T
	t.Helper()
	sb.prepareCodex(RouteAPIKey)
	args := append([]string{"-C", sb.workDir(), "-c", `cli_auth_credentials_store="file"`, "-c", "features.plugins=false",
		"-c", "features.remote_plugin=false", "--dangerously-bypass-hook-trust"}, fixtureCodexArgs(providerURL)...)
	term, err := Start(sb.workDir(), sb.codexEnv(RouteAPIKey), 40, 140, b.Path, append(args, prompt)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.Expect(regexp.MustCompile(`TERMA_TELEMETRY_REPLY`), 90*time.Second); err != nil {
		t.Fatalf("the TUI never replied:\n%s", tail(term.Text(), 3000))
	}
	time.Sleep(12 * time.Second) // the title conversation starts after the first reply
	_ = term.Close("/quit", 8*time.Second)
}

// replyingCodexProvider answers every request with TERMA_TELEMETRY_REPLY — the
// thread's turn and the TUI's title request alike.
func replyingCodexProvider(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		item := map[string]any{"type": "message", "id": "msg_reply", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}}}
		writeCodexResponse(w, calls.Add(1), item)
	})
}

// The Codex TUI through the relay. After the first reply it starts a conversation of
// its own to title the thread — its own conversation.id, its own model call and cost,
// no hook, nothing linking it to the thread. Nothing proves it is not the developer's,
// so it never leaves: only the claimed thread does. The process named two
// conversations, so its metrics are dropped too (the thread's usage still arrives, in
// its logs).
func TestRelayCodexTUITitle(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(replyingCodexProvider(&calls))
		defer provider.Close()
		sb.codexTUI(b, provider.URL, "Say hello please")
		time.Sleep(5 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		e := sb.Receiver.evidence()
		threads := map[string]bool{}
		var title string
		for _, r := range e.logs {
			if r.Attrs["event.name"] != "codex.conversation_starts" {
				continue
			}
			threads[r.Attrs["conversation.id"]] = true
			if r.Attrs["approval_policy"] == "never" && r.Attrs["sandbox_policy"] == "read-only" {
				title = r.Attrs["conversation.id"]
			}
		}
		switch {
		case calls.Load() < 2:
			Note(t.Name(), fmt.Sprintf("the TUI made %d model call(s): no title conversation this build", calls.Load()))
		case title != "":
			t.Errorf("the unclaimed title conversation %s reached upstream", title)
		case len(threads) != 1:
			t.Errorf("want only the claimed thread upstream, got conversations %v", threads)
		}
		for _, r := range e.logs {
			if !fromTerma(r) && r.Resource["mirador.project.id"] != sb.ProjectID {
				t.Errorf("a record reached upstream without the project: %v", r.Attrs)
			}
		}
		// What was dropped is the title conversation's, and the process's work that names
		// no session; the claimed thread lost nothing.
		if n := c["dropped.uncovered_process.logs"] + c["dropped.no_key.logs"]; n > 0 {
			t.Errorf("records of the claimed thread were dropped: %v", c)
		}
		if n := sum(c, "attributed_by_process."); n > 0 {
			t.Errorf("a process that named two conversations had %d records attributed to one: %v", n, c)
		}
	})
}
