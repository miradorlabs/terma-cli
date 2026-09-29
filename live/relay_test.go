package live

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The relay, end to end (docs/RELAY.md): real Claude Code and Codex builds export
// through their global configuration to `terma relay serve` on loopback, the repository's
// hooks record which directory each session runs in, and the receiver stands in for
// Terma upstream. Every project's records arrive under that project's key, so
// evidenceFor(key) is exactly what the relay delivered to that project. What these prove:
//
//   - a session in a bound repository arrives whole at that repository's project: every
//     contract the direct export meets, under the project's key, and nothing of it
//     anywhere else;
//   - a session in no bound repository, and whatever names no session (Codex's metrics),
//     arrives at the machine project instead;
//   - content leaves only as the project's policy allows;
//   - nothing accepted is lost: not to a relay killed mid-session, not to an outage of
//     Terma's ingest.

// relayHold is how long the relay waits for a session to be placed before it gives the
// session's records to the machine project (relay.DefaultHold).
const relayHold = 30 * time.Second

func bearer(key string) string { return "Bearer " + key }

// isAgent is a record an agent exported, not one terma's own hooks delivered.
func isAgent(res map[string]string) bool { return res["service.name"] != "terma-cli" }

// sessionOf is the session a record names, the way the relay reads it: Claude's
// session.id, Codex's conversation.id, or a Codex span's thread.id when it is the thread's
// id and not an OS thread number.
func sessionOf(attrs map[string]string) string {
	if v := attrs["session.id"]; v != "" {
		return v
	}
	if v := attrs["conversation.id"]; v != "" {
		return v
	}
	for _, k := range []string{"thread.id", "thread_id"} {
		if v := attrs[k]; len(v) == 36 {
			return v
		}
	}
	return ""
}

// agentSessions counts, per session named, the agent records in e.
func agentSessions(e telemetryEvidence) map[string]int {
	out := map[string]int{}
	for _, r := range e.logs {
		if isAgent(r.Resource) {
			out[sessionOf(r.Attrs)]++
		}
	}
	for _, s := range e.spans {
		out[sessionOf(s.Attrs)]++
	}
	for _, m := range e.metrics {
		for _, p := range dataPointAttrs(m) {
			out[sessionOf(p)]++
		}
	}
	return out
}

// dataPointAttrs are a metric's data points' attributes.
func dataPointAttrs(m Metric) []map[string]string {
	var out []map[string]string
	mp := m.Proto
	switch {
	case mp.GetSum() != nil:
		for _, p := range mp.GetSum().GetDataPoints() {
			out = append(out, flatten(p.GetAttributes()))
		}
	case mp.GetGauge() != nil:
		for _, p := range mp.GetGauge().GetDataPoints() {
			out = append(out, flatten(p.GetAttributes()))
		}
	case mp.GetHistogram() != nil:
		for _, p := range mp.GetHistogram().GetDataPoints() {
			out = append(out, flatten(p.GetAttributes()))
		}
	case mp.GetExponentialHistogram() != nil:
		for _, p := range mp.GetExponentialHistogram().GetDataPoints() {
			out = append(out, flatten(p.GetAttributes()))
		}
	}
	return out
}

// checkOnlySession fails if what reached a project under its key names any session
// but sid: the relay gave it another session's records.
func checkOnlySession(t contractReporter, e telemetryEvidence, sid, where string) {
	t.Helper()
	for s, n := range agentSessions(e) {
		if s != "" && s != sid {
			t.Errorf("%s received %d records of session %s, which is not its session %s", where, n, s, sid)
		}
	}
}

// checkNoSession fails if any record naming sid reached where.
func checkNoSession(t contractReporter, e telemetryEvidence, sid, where string) {
	t.Helper()
	if n := agentSessions(e)[sid]; n > 0 {
		t.Errorf("%s received %d records of session %s, which belongs elsewhere", where, n, sid)
	}
}

func noteRelay(name string, sb *Sandbox) {
	h := sb.RelayStats()
	keys := make([]string, 0, len(h.Counters))
	for k := range h.Counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, h.Counters[k]))
	}
	if h.LastError != "" {
		parts = append(parts, "last error: "+h.LastError)
	}
	Note(name, "relay: "+strings.Join(parts, ", "))
}

// leakedFields names every span, span event and log field that carries one of the
// scenario's content markers: what to add to the relay's withheld fields when a harness
// release starts exporting content somewhere new.
func leakedFields(e telemetryEvidence) []string {
	return leakedFieldsOf(e, telemetryPrompt, "TERMA_TELEMETRY_REPLY", "TERMA_TELEMETRY_TOOL")
}

// leakedFieldsOf is leakedFields for the given markers.
func leakedFieldsOf(e telemetryEvidence, markers ...string) []string {
	var out []string
	hit := func(where, k, v string) {
		for _, m := range markers {
			if strings.Contains(v, m) {
				out = append(out, where+"."+k)
				return
			}
		}
	}
	for _, s := range e.spans {
		for k, v := range s.Attrs {
			hit("span "+s.Name, k, v)
		}
		for _, ev := range s.Proto.GetEvents() {
			for k, v := range flatten(ev.GetAttributes()) {
				hit("span "+s.Name+" event "+ev.GetName(), k, v)
			}
		}
	}
	for _, r := range e.logs {
		for k, v := range r.Attrs {
			hit("log "+r.Attrs["event.name"], k, v)
		}
		hit("log "+r.Attrs["event.name"], "body", r.Body)
	}
	sort.Strings(out)
	return out
}

func relayMode(content bool) string {
	return map[bool]string{true: "content", false: "withheld"}[content]
}

// claudeRun is one deterministic Claude Code session in dir: a Bash tool call, then the
// reply marker.
func claudeRun(t *testing.T, sb *Sandbox, dir string) string {
	t.Helper()
	var calls atomic.Int32
	provider := httptest.NewServer(claudeTelemetryProvider(&calls))
	defer provider.Close()
	sb.ClaudeBaseURL = provider.URL
	_, sid := sb.ClaudeHeadlessIn(dir, RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
	return sid
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// Claude Code through the relay: the full contract of a direct export, under the bound
// project's key, and nothing of the session anywhere else.
func TestRelayClaude(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				sb.UseRelay(RelayOptions{Content: content})
				sid := claudeRun(t, sb, sb.Repo)
				awaitTelemetry(t, sb, func(r contractReporter, _ telemetryEvidence) {
					e := sb.Receiver.evidenceFor(bearer(liveKey))
					checkClaudeTelemetry(r, e, sid, sb.ProjectID, !content)
					checkOnlySession(r, e, sid, "the bound project")
				})
				all := sb.Receiver.evidence()
				checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), sid, "the machine project")
				if !content {
					if leaked := leakedFields(all); len(leaked) > 0 {
						t.Errorf("content withheld, but it reached Terma in: %v", leaked)
					}
				}
				checkTelemetrySchemaAt(t, sb.Receiver.evidenceFor(bearer(liveKey)), "claude", "relay/claude-"+relayMode(content), newest)
				noteRelay(t.Name(), sb)
			})
		}
	})
}

// Codex through the relay. Its session's logs and spans reach the bound project, its
// child spans through their trace; its metrics name no session and reach the machine
// project instead, and nothing naming the session reaches the machine project.
func TestRelayCodex(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithCodex(b))
				sb.UseRelay(RelayOptions{Content: content})
				run := codexRun(t, sb)
				awaitTelemetry(t, sb, func(r contractReporter, _ telemetryEvidence) {
					e := sb.Receiver.evidenceFor(bearer(liveKey))
					checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, !content, true)
					checkOnlySession(r, e, run.ThreadID, "the bound project")
					checkCodexMetricsAtMachine(r, sb.Receiver.evidenceFor(bearer(machineKey)))
				})
				checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), run.ThreadID, "the machine project")
				if !content {
					if leaked := leakedFields(sb.Receiver.evidence()); len(leaked) > 0 {
						t.Errorf("content withheld, but it reached Terma in: %v", leaked)
					}
				}
				checkTelemetrySchemaAt(t, sb.Receiver.evidenceFor(bearer(liveKey)), "codex", "relay/codex-"+relayMode(content), newest)
				noteRelay(t.Name(), sb)
			})
		}
	})
}

// checkCodexMetricsAtMachine requires Codex's usage metrics at the machine project: they
// name no session, so the relay cannot place them anywhere else, and dropping them would
// lose them.
func checkCodexMetricsAtMachine(t contractReporter, e telemetryEvidence) {
	t.Helper()
	for _, name := range []string{"codex.turn.token_usage", "codex.api_request"} {
		found := false
		for _, m := range e.metrics {
			if m.Proto.GetName() == name {
				found = true
			}
		}
		if !found {
			t.Errorf("the machine project never received Codex's %s", name)
		}
	}
}

// codexRun is one deterministic Codex turn in the sandbox's working directory: a shell
// tool call, then the reply marker.
func codexRun(t *testing.T, sb *Sandbox) *CodexRun {
	t.Helper()
	var calls atomic.Int32
	provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
	defer provider.Close()
	run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Codex stdout:\n%s\nCodex stderr:\n%s", run.Stdout, run.Stderr)
		}
	})
	return run
}

// A session no bound repository placed goes to the machine project, whole, once its hold
// closes: one outside any repository, one in a repository without a binding, and one in
// the bound repository with terma's hooks switched off (TERMA_HOOKS=0 stops what the
// hooks capture, not the agents' own export). None of it reaches the bound project.
func TestRelayUnplacedSessionsReachTheMachineProject(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		for _, where := range []string{"outside-repo", "unbound-repo", "hooks-off"} {
			t.Run(where, func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				dir := sb.Repo
				switch where {
				case "outside-repo":
					dir = filepath.Join(sb.Dir, "personal")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				case "unbound-repo":
					dir = filepath.Join(sb.Dir, "unbound")
					sb.newRepo(dir)
				case "hooks-off":
					sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_HOOKS=0")
				}
				sb.UseRelay(RelayOptions{Content: true})
				sid := claudeRun(t, sb, dir)
				machine := func() telemetryEvidence { return sb.Receiver.evidenceFor(bearer(machineKey)) }
				// Past the hold: the relay places a session it never heard of only then.
				if !waitFor(relayHold+30*time.Second, func() bool {
					return agentSessions(machine())[sid] > 0 && sb.relayDrained(time.Second)
				}) {
					t.Fatalf("the unplaced session never reached the machine project; relay: %+v\nlog:\n%s", sb.RelayStats(), sb.relayLog())
				}
				var failures contractFailures
				e := machine()
				// The whole native contract of the session, at the machine project. The hook
				// lifecycle is the bound repository's alone, so it is not asked for here.
				for _, name := range []string{"user_prompt", "api_request", "assistant_response", "tool_result"} {
					e.logsFor(&failures, name, "session.id", sid, 1)
				}
				checkSpans(&failures, e, "session.id", sid, "", "claude_code.interaction", "claude_code.llm_request", "claude_code.tool")
				checkSumMetric(&failures, e, "claude_code.token.usage", map[string]string{"session.id": sid, "type": "input"}, 24, "")
				for _, f := range failures {
					t.Error(f)
				}
				checkNoSession(t, sb.Receiver.evidenceFor(bearer(liveKey)), sid, "the bound project")
				noteRelay(t.Name(), sb)
			})
		}
	})
}

// Two bound repositories in different projects and a personal session, at once, through
// one relay: each bound session reaches its own project under its own key, the personal
// one the machine project, and no session's records reach another's project.
func TestRelayConcurrentProjects(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		other := sb.InstallOther()
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		sids := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, dir := range map[string]string{"live": sb.Repo, "other": other, "personal": personal} {
			wg.Go(func() {
				// One provider serves all three; whichever asks first is given the tool, so
				// the sessions' shapes differ, but every one exports.
				_, sid := sb.ClaudeHeadlessIn(dir, RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
				mu.Lock()
				sids[name] = sid
				mu.Unlock()
			})
		}
		wg.Wait()
		where := map[string]string{"live": liveKey, "other": otherKey, "personal": machineKey}
		ok := waitFor(relayHold+30*time.Second, func() bool {
			for name, key := range where {
				if agentSessions(sb.Receiver.evidenceFor(bearer(key)))[sids[name]] == 0 {
					return false
				}
			}
			return sb.relayDrained(time.Second)
		})
		for name, key := range where {
			e := sb.Receiver.evidenceFor(bearer(key))
			if agentSessions(e)[sids[name]] == 0 {
				t.Errorf("the %s session never reached its project (key %s)", name, key)
			}
			for other, sid := range sids {
				if other != name {
					checkNoSession(t, e, sid, "the "+name+" session's project")
				}
			}
		}
		if !ok {
			t.Logf("relay: %+v\nlog:\n%s", sb.RelayStats(), sb.relayLog())
		}
		noteRelay(t.Name(), sb)
	})
}

// A developer who has not trusted the repository's Codex hooks yet: no hook runs, but the
// relay places the session from Codex's own rollout (session_meta.cwd), so it still
// reaches the bound project.
func TestRelayCodexUntrustedHooks(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.CodexHooksUntrusted = true
		sb.UseRelay(RelayOptions{Content: true})
		run := codexRun(t, sb)
		if !waitFor(45*time.Second, func() bool {
			return agentSessions(sb.Receiver.evidenceFor(bearer(liveKey)))[run.ThreadID] > 0 && sb.relayDrained(time.Second)
		}) {
			t.Fatalf("a session no hook announced never reached the bound project; relay: %+v", sb.RelayStats())
		}
		if len(sb.HookPayloads("codex-session-start")) != 0 {
			t.Fatalf("a hook ran, so this does not prove the rollout placed the session")
		}
		var failures contractFailures
		e := sb.Receiver.evidenceFor(bearer(liveKey))
		for _, name := range []string{"codex.user_prompt", "codex.api_request", "codex.tool_result"} {
			e.logsFor(&failures, name, "conversation.id", run.ThreadID, 1)
		}
		for _, f := range failures {
			t.Error(f)
		}
		checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), run.ThreadID, "the machine project")
		noteRelay(t.Name(), sb)
	})
}

// A session in a linked worktree (git worktree add) belongs to its main checkout's
// project: the binding is inherited, so its telemetry reaches that project.
func TestRelayLinkedWorktree(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		// The hooks are committed, as they are once the install is merged; a worktree
		// checks out what is committed, and the binding (ignored or not) stays behind.
		sb.git("add", "-A")
		sb.git("commit", "-q", "-m", "install terma")
		wt := filepath.Join(sb.Dir, "wt")
		sb.git("worktree", "add", "-q", wt)
		_ = os.Remove(filepath.Join(wt, ".terma", "settings.json"))
		sid := claudeRun(t, sb, wt)
		if !waitFor(45*time.Second, func() bool {
			return agentSessions(sb.Receiver.evidenceFor(bearer(liveKey)))[sid] > 0 && sb.relayDrained(time.Second)
		}) {
			t.Fatalf("a linked worktree's session never reached the main checkout's project; relay: %+v", sb.RelayStats())
		}
		checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), sid, "the machine project")
		noteRelay(t.Name(), sb)
	})
}

// A Claude subagent (the Agent tool) runs inside the parent's session: its requests are
// the parent session's, and reach the parent's project with it.
func TestRelayClaudeSubagent(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		steps := []map[string]any{{"name": "Agent", "input": map[string]any{"description": "check", "prompt": "Reply exactly TERMA_SUBAGENT_REPLY.", "subagent_type": "general-purpose"}}}
		var calls atomic.Int32
		provider := httptest.NewServer(claudeScriptedProvider(&calls, steps))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, "Use a subagent.", "--max-turns", "4", "--tools", "Agent", "--allowedTools", "Agent")
		if calls.Load() < 3 {
			t.Fatalf("provider calls = %d: no subagent ran", calls.Load())
		}
		apiRequests := func() int {
			n := 0
			for _, r := range sb.Receiver.evidenceFor(bearer(liveKey)).logs {
				if r.Attrs["event.name"] == "api_request" && r.Attrs["session.id"] == sid {
					n++
				}
			}
			return n
		}
		if !waitFor(45*time.Second, func() bool { return apiRequests() >= 3 }) {
			t.Errorf("want the parent's and the subagent's api_request records at the bound project, got %d", apiRequests())
		}
		checkOnlySession(t, sb.Receiver.evidenceFor(bearer(liveKey)), sid, "the bound project")
		checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), sid, "the machine project")
		noteRelay(t.Name(), sb)
	})
}

// A session whose directory is recorded after its first exports — a hook that ran late —
// is placed then, as long as the record lands inside the hold: the records waited, and
// go to the recorded directory's project, none to the machine project.
func TestRelayLateSessionRecord(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		// The session runs where no hook records it, then the record is written the way the
		// session-start hook writes it, naming the bound repository.
		dir := filepath.Join(sb.Dir, "elsewhere")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		sid := claudeRun(t, sb, dir)
		if n := agentSessions(sb.Receiver.evidence())[sid]; n != 0 {
			t.Fatalf("%d records reached Terma before the session was placed", n)
		}
		if time.Since(start) > relayHold-5*time.Second {
			t.Skipf("the run took %s, too close to the hold to place the session inside it", time.Since(start))
		}
		sb.writeAbs(filepath.Join(sb.TermaConfig, "relay", "sessions", sid), sb.Repo+"\n")
		if !waitFor(relayHold, func() bool {
			return agentSessions(sb.Receiver.evidenceFor(bearer(liveKey)))[sid] > 0 && sb.relayDrained(time.Second)
		}) {
			t.Fatalf("the late record did not place the session; relay: %+v", sb.RelayStats())
		}
		checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), sid, "the machine project")
		noteRelay(t.Name(), sb)
	})
}
