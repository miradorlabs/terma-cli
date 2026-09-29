package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	for _, m := range e.metrics {
		for _, p := range m.Proto.GetSum().GetDataPoints() {
			for k, v := range flatten(p.Attributes) {
				hit("metric "+m.Proto.GetName(), k, v)
			}
		}
		for _, p := range m.Proto.GetHistogram().GetDataPoints() {
			for k, v := range flatten(p.Attributes) {
				hit("metric "+m.Proto.GetName(), k, v)
			}
		}
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
				checkFieldRegistry(t, all, "claude", b.Version, !content, newest)
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
				checkFieldRegistry(t, sb.Receiver.evidence(), "codex", b.Version, !content, newest)
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
		entries, _ := os.ReadDir(sb.payloadDir())
		var ran []string
		for _, e := range entries {
			ran = append(ran, e.Name())
		}
		t.Logf("hooks that ran: %v", ran)
		// No hook ran, so the session can only have been placed from Codex's rollout. Every
		// record naming the session must reach the bound project; the relay's backlog is not
		// asked to drain, because Codex's process spans correctly wait out the trace hold.
		check := func(r contractReporter) {
			e := sb.Receiver.evidenceFor(bearer(liveKey))
			for _, name := range []string{"codex.user_prompt", "codex.api_request", "codex.tool_result"} {
				e.logsFor(r, name, "conversation.id", run.ThreadID, 1)
			}
		}
		waitFor(45*time.Second, func() bool {
			var f contractFailures
			check(&f)
			return len(f) == 0
		})
		var failures contractFailures
		check(&failures)
		for _, f := range failures {
			t.Error(f)
		}
		if t.Failed() {
			t.Logf("relay: %+v\n%s", sb.RelayStats(), sb.routingReport(run.ThreadID))
		}
		for _, p := range ran {
			if strings.Contains(p, "codex-session-start") {
				t.Errorf("a repository hook ran (%s), so this does not prove the rollout placed the session", p)
			}
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
		// The session runs where no hook records it — setup's user-level placement hook
		// taken out, as on a machine where it failed to run — then the record is written
		// the way the session-start hook writes it, naming the bound repository.
		sb.dropClaudePlacementHook()
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

// What the relay accepted is on disk before it answers, so a relay killed outright while
// Terma's ingest is refusing everything loses nothing: the restarted relay delivers the
// session whole, and terma's own hook events follow with the next flush.
func TestRelayKeepsWhatItAcceptedAcrossACrash(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		sb.Receiver.Refuse(503)
		sid := claudeRun(t, sb, sb.Repo)
		// Everything the session exported is accepted and waiting.
		if !waitFor(30*time.Second, func() bool {
			h := sb.RelayStats()
			return h.Counters["received"] > 0 && h.Backlog > 0 && sb.Receiver.Refused() > 0
		}) {
			t.Fatalf("the relay never held the session while Terma refused it: %+v", sb.RelayStats())
		}
		held := sb.RelayStats().Backlog
		sb.KillRelay()
		if n := agentSessions(sb.Receiver.evidence())[sid]; n != 0 {
			t.Fatalf("%d records reached Terma while it was refusing", n)
		}
		sb.Receiver.Refuse(0)
		sb.StartRelay() // what launchd or systemd does after a crash
		sb.terma(sb.Repo, "spool", "flush", "--force")
		awaitTelemetry(t, sb, func(r contractReporter, _ telemetryEvidence) {
			checkClaudeTelemetry(r, sb.Receiver.evidenceFor(bearer(liveKey)), sid, sb.ProjectID, false)
		})
		Note(t.Name(), fmt.Sprintf("crash: %d bodies held across SIGKILL, %d refused exports before it", held, sb.Receiver.Refused()))
		noteRelay(t.Name(), sb)
	})
}

// An outage of Terma's ingest that outlasts the session: the relay keeps retrying, with
// backoff, and delivers the session whole once ingest is back — without a restart.
func TestRelayDeliversAfterAnOutage(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		sb.Receiver.Refuse(503)
		sid := claudeRun(t, sb, sb.Repo)
		time.Sleep(15 * time.Second) // several failed attempts: the backoff has grown
		if sb.Receiver.Refused() == 0 {
			t.Fatal("the relay never tried Terma during the outage")
		}
		sb.Receiver.Refuse(0)
		sb.terma(sb.Repo, "spool", "flush", "--force")
		// The relay's backoff tops out at two minutes (docs/RELAY.md).
		deadline := time.Now().Add(2*time.Minute + 30*time.Second)
		for time.Now().Before(deadline) {
			var failures contractFailures
			checkClaudeTelemetry(&failures, sb.Receiver.evidenceFor(bearer(liveKey)), sid, sb.ProjectID, false)
			if len(failures) == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		awaitTelemetry(t, sb, func(r contractReporter, _ telemetryEvidence) {
			checkClaudeTelemetry(r, sb.Receiver.evidenceFor(bearer(liveKey)), sid, sb.ProjectID, false)
		})
		noteRelay(t.Name(), sb)
	})
}

// An interactive session (the way developers use Claude) whose relay dies between turns
// and is started again, as its service manager would, before the next turn: every turn
// arrives, the one before the crash included.
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
		sb.UseRelay(RelayOptions{Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeScriptedProvider(&calls, nil))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		reply := regexp.MustCompile(`TERMA_TELEMETRY_REPLY`)
		prompts := []string{"turn one", "turn two", "turn three"}
		run := sb.ClaudeInteractiveTurns(RouteAPIKey, prompts, []*regexp.Regexp{reply, reply, reply}, func(turn int, _ string) {
			if turn == 0 {
				sb.KillRelay()
				sb.StartRelay()
			}
		})
		perTurn := func() []int {
			counts := make([]int, len(prompts))
			for _, r := range sb.Receiver.evidenceFor(bearer(liveKey)).logs {
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
		waitFor(45*time.Second, func() bool { c := perTurn(); return c[0] > 0 && c[1] > 0 && c[2] > 0 })
		if c := perTurn(); c[0] == 0 || c[1] == 0 || c[2] == 0 {
			t.Errorf("user_prompt records per turn %v: every turn must arrive, the one before the crash included", c)
		}
		checkOnlySession(t, sb.Receiver.evidenceFor(bearer(liveKey)), run.SessionID, "the bound project")
		noteRelay(t.Name(), sb)
	})
}

// A Codex turn that outlasts the relay's 30-second session hold: the provider stalls
// mid-turn, so the turn's first child spans are exported long before the turn span that
// names the session. They wait under their trace (the trace hold) and the full contract
// still reaches the session's project.
func TestRelayCodexLongTurn(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Content: true})
		var calls atomic.Int32
		inner := codexTelemetryProvider(t, &calls)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Load() >= 1 {
				time.Sleep(relayHold + 5*time.Second) // the second request: past the session hold
			}
			inner.ServeHTTP(w, r)
		}))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		awaitTelemetry(t, sb, func(r contractReporter, _ telemetryEvidence) {
			e := sb.Receiver.evidenceFor(bearer(liveKey))
			checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, false, true)
			checkOnlySession(r, e, run.ThreadID, "the bound project")
		})
		checkNoSession(t, sb.Receiver.evidenceFor(bearer(machineKey)), run.ThreadID, "the machine project")
		noteRelay(t.Name(), sb)
	})
}

// File tools carry file contents: Write's input, Read's output, Edit's strings. With
// tool content withheld none of it may leave, wherever a Claude release puts it; with
// it allowed, the relay must not have removed it.
func TestRelayClaudeFileToolsContent(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		for _, content := range []bool{false, true} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				sb.UseRelay(RelayOptions{Content: content})
				path := filepath.Join(sb.Repo, "secret.txt")
				steps := []map[string]any{
					{"name": "Write", "input": map[string]any{"file_path": path, "content": fileSecret + " line one\n"}},
					{"name": "Read", "input": map[string]any{"file_path": path}},
					{"name": "Edit", "input": map[string]any{"file_path": path, "old_string": "line one", "new_string": fileSecret + " edited"}},
				}
				var calls atomic.Int32
				provider := httptest.NewServer(claudeScriptedProvider(&calls, steps))
				defer provider.Close()
				sb.ClaudeBaseURL = provider.URL
				_, sid := sb.ClaudeHeadless(RouteAPIKey, "Write, read and edit the file.", "--max-turns", "6", "--tools", "Write,Read,Edit", "--allowedTools", "Write,Read,Edit", "--permission-mode", "acceptEdits")
				if calls.Load() < int32(len(steps))+1 {
					t.Fatalf("provider calls = %d: Claude did not run the scripted tools", calls.Load())
				}
				waitFor(30*time.Second, func() bool {
					return len(sb.APIRequests(sid, time.Second)) >= len(steps)+1 && sb.relayDrained(time.Second)
				})
				found := leakedFieldsOf(sb.Receiver.evidence(), fileSecret)
				if content && len(found) == 0 {
					t.Errorf("tool content allowed, but the file's contents reached Terma nowhere")
				}
				if !content && len(found) > 0 {
					t.Errorf("tool content withheld, but the file's contents reached Terma in: %v", found)
				}
				checkOnlySession(t, sb.Receiver.evidenceFor(bearer(liveKey)), sid, "the bound project")
				if content {
					Note(t.Name(), "file contents at Terma in: "+strings.Join(found, ", "))
				}
				noteRelay(t.Name(), sb)
			})
		}
	})
}

// A session placed in the bound repository, resumed somewhere personal: Claude keeps the
// session id, and the relay keeps a session with the project it was first placed in, so
// the resumed run reaches the bound project too. This records it rather than failing on
// it: telling the resumed run apart needs per-process placement (PR #27's), which PR #28
// does not have.
func TestRelayClaudeResumedElsewhere(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Content: true})
		// One provider for the run and its resumption.
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		const personalPrompt = "TERMA_PERSONAL_WORK please"
		resumed, err := sb.resumeClaude(personal, sid, personalPrompt)
		if err != nil {
			Note(t.Name(), "resume elsewhere: Claude refused ("+strings.SplitN(err.Error(), "\n", 2)[0]+")")
			return
		}
		waitFor(relayHold+15*time.Second, func() bool {
			return len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_PERSONAL_WORK")) > 0 && sb.relayDrained(time.Second)
		})
		at := map[string]int{}
		for name, key := range map[string]string{"bound project": liveKey, "machine project": machineKey} {
			at[name] = len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(key)), "TERMA_PERSONAL_WORK"))
		}
		msg := fmt.Sprintf("resume elsewhere: session id %s; the personal run's prompt reached the bound project in %d fields, the machine project in %d",
			map[bool]string{true: "kept", false: "changed"}[resumed == sid], at["bound project"], at["machine project"])
		t.Log(msg)
		noteRelay(t.Name(), sb)
		// The user-level placement hook records the resumed run's directory, and the relay
		// places each record by the placement in effect when it happened: the personal
		// run goes to the machine project, and none of it to the repository's.
		if at["bound project"] != 0 {
			t.Fatalf("%s\n%s", msg, sb.routingReport(sid))
		}
		if at["machine project"] == 0 {
			t.Fatalf("the personal run's prompt reached neither project: %s\n%s", msg, sb.routingReport(sid))
		}
		if n := agentSessions(sb.Receiver.evidenceFor(bearer(liveKey)))[sid]; n == 0 {
			t.Fatalf("the original run's records left the repository's project:\n%s", sb.routingReport(sid))
		}
	})
}

// routingReport says where a session's records went and what the relay decided, for a
// failure message.
func (sb *Sandbox) routingReport(sid string) string {
	var b strings.Builder
	for name, key := range map[string]string{"bound": liveKey, "machine": machineKey, "other": otherKey} {
		fmt.Fprintf(&b, "at %s: %v\n", name, agentSessions(sb.Receiver.evidenceFor(bearer(key))))
	}
	for _, sub := range []string{"sessions", "routes"} {
		entries, _ := os.ReadDir(filepath.Join(sb.TermaConfig, "relay", sub))
		for _, e := range entries {
			data, _ := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", sub, e.Name()))
			fmt.Fprintf(&b, "%s/%s: %s", sub, e.Name(), data)
		}
	}
	fmt.Fprintf(&b, "session under test: %s\nlog:\n%s", sid, sb.relayLog())
	return b.String()
}

// `codex exec resume` in a personal directory keeps the thread. Codex runs no user-level
// hooks, so the relay learns the resumed turn's directory from the rollout's turn_context:
// the personal turn goes to the machine project, the first turn stays with the repository.
func TestRelayCodexResumedElsewhere(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := sb.resumeCodex(personal, run.ThreadID, "TERMA_PERSONAL_WORK please", fixtureCodexArgs(provider.URL)...); err != nil {
			Note(t.Name(), "resume elsewhere: Codex refused ("+strings.SplitN(err.Error(), "\n", 2)[0]+")")
			return
		}
		waitFor(relayHold+15*time.Second, func() bool {
			return len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_PERSONAL_WORK")) > 0 && sb.relayDrained(time.Second)
		})
		bound := len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(liveKey)), "TERMA_PERSONAL_WORK"))
		machine := len(leakedFieldsOf(sb.Receiver.evidenceFor(bearer(machineKey)), "TERMA_PERSONAL_WORK"))
		msg := fmt.Sprintf("codex resume elsewhere: the personal turn's prompt reached the bound project in %d fields, the machine project in %d", bound, machine)
		t.Log(msg)
		noteRelay(t.Name(), sb)
		if bound != 0 {
			t.Fatalf("%s\n%s", msg, sb.routingReport(run.ThreadID))
		}
		if machine == 0 {
			t.Fatalf("the personal turn's prompt reached neither project: %s\n%s", msg, sb.routingReport(run.ThreadID))
		}
		if n := agentSessions(sb.Receiver.evidenceFor(bearer(liveKey)))[run.ThreadID]; n == 0 {
			t.Fatalf("the first turn's records left the repository's project:\n%s", sb.routingReport(run.ThreadID))
		}
	})
}

// dropClaudePlacementHook removes terma's user-level placement hook from the sandbox's
// Claude Code settings, for a scenario about a session no hook records.
func (sb *Sandbox) dropClaudePlacementHook() {
	sb.T.Helper()
	path := filepath.Join(sb.ClaudeConfig, "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		sb.T.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		sb.T.Fatal(err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	delete(hooks, "SessionStart")
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		sb.T.Fatal(err)
	}
	if strings.Contains(string(out), "terma hook place") {
		sb.T.Fatal("the placement hook is still in the sandbox's Claude settings")
	}
	sb.writeAbs(path, string(out)+"\n")
}
