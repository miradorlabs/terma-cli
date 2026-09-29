package live

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The local relay spike (docs/RELAY-SPIKE.md), end to end: real Claude Code and Codex
// builds export through their global configuration to `terma relay run` on loopback,
// hooks in the installed repository claim their sessions, and the receiver stands in
// for Terma upstream. What these prove:
//
//   - an opted-in session arrives whole: every contract the direct export meets, the
//     project stamped on it, the project's key on every request;
//   - content leaves only as the project's routing record allows;
//   - nothing else reaches upstream — a session outside any repository, in a
//     repository with no binding, or in one this machine holds no key for;
//   - how a cold start (relay not running when the agent starts) goes.

// checkOnlyClaimed fails if anything the agent exported reached upstream without
// belonging to the claimed session and project. terma's own spooled events
// (service.name=terma-cli) are the hooks' delivery, not the relay's, and are skipped.
func checkOnlyClaimed(t contractReporter, e telemetryEvidence, key, sid, project string) {
	t.Helper()
	agent := func(res map[string]string) bool { return res["service.name"] != "terma-cli" }
	for _, r := range e.logs {
		if !agent(r.Resource) {
			continue
		}
		if r.Attrs[key] != sid || r.Resource["mirador.project.id"] != project {
			t.Errorf("relay forwarded a log that is not the claimed session's: %s %s=%q project=%q", r.Attrs["event.name"], key, r.Attrs[key], r.Resource["mirador.project.id"])
		}
	}
	for _, s := range e.spans {
		if agent(s.Resource) && s.Resource["mirador.project.id"] != project {
			t.Errorf("relay forwarded span %s without the claimed project", s.Name)
		}
		for _, k := range []string{"session.id", "conversation.id", "thread.id"} {
			// A numeric thread.id is Codex's OS thread number, not a session.
			if v := s.Attrs[k]; v != "" && v != sid && !(k == "thread.id" && isDigits(v)) {
				t.Errorf("relay forwarded span %s of another session: %s=%q", s.Name, k, v)
			}
		}
	}
	for _, m := range e.metrics {
		if m.Resource["mirador.project.id"] != project {
			t.Errorf("relay forwarded metric %s without the claimed project", m.Proto.GetName())
		}
	}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// agentRecords counts what reached upstream from an agent rather than from terma's
// own hook delivery.
func agentRecords(e telemetryEvidence) int {
	n := len(e.spans) + len(e.metrics)
	for _, r := range e.logs {
		if r.Resource["service.name"] != "terma-cli" {
			n++
		}
	}
	return n
}

func noteRelayStats(name string, c map[string]int) {
	var parts []string
	for _, k := range []string{"received.", "forwarded.", "dropped.no_session_id.", "dropped.no_session_trace", "dropped.unclaimed_expired.", "dropped.no_key.", "released_after_hold", "withheld_content_records", "upstream_retries"} {
		if n := sum(c, k); n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", strings.TrimSuffix(k, "."), n))
		}
	}
	Note(name, "relay: "+strings.Join(parts, " "))
}

// leakedFields names every span, span event and log field upstream received that
// carries one of the scenario's content markers: what to add to the relay's withheld
// fields when a harness release starts exporting content somewhere new.
func leakedFields(e telemetryEvidence) []string {
	markers := []string{telemetryPrompt, "TERMA_TELEMETRY_REPLY", "TERMA_TELEMETRY_TOOL"}
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
	return out
}

func relayMode(content bool) string {
	return map[bool]string{true: "content", false: "withheld"}[content]
}

// Claude Code through the relay: the full contract of a direct export, only the
// claimed session, the project on every resource.
func TestRelayClaude(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, newest bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				var calls atomic.Int32
				provider := httptest.NewServer(claudeTelemetryProvider(&calls))
				defer provider.Close()
				sb.ClaudeBaseURL = provider.URL
				_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
				awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
					checkClaudeTelemetry(r, e, sid, sb.ProjectID, !content)
					checkOnlyClaimed(r, e, "session.id", sid, sb.ProjectID)
				})
				if !content && t.Failed() {
					t.Logf("content upstream received: %v", leakedFields(sb.Receiver.evidence()))
				}
				checkTelemetrySchemaAt(t, sb.Receiver.evidence(), "claude", "relay/claude-"+relayMode(content), newest)
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				if n := sum(c, "dropped.unclaimed"); n > 0 {
					t.Errorf("an opted-in session lost %d records waiting for its claim: %v", n, c)
				}
			})
		}
	})
}

// Codex through the relay. Its metrics name no session and never pass; its child
// spans pass through their trace.
func TestRelayCodex(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, newest bool) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithCodex(b))
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				var calls atomic.Int32
				provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
				defer provider.Close()
				run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
				t.Cleanup(func() {
					if t.Failed() {
						t.Logf("Codex stdout:\n%s\nCodex stderr:\n%s", run.Stdout, run.Stderr)
					}
				})
				awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
					checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, !content, true)
					checkOnlyClaimed(r, e, "conversation.id", run.ThreadID, sb.ProjectID)
				})
				if !content && t.Failed() {
					t.Logf("content upstream received: %v", leakedFields(sb.Receiver.evidence()))
				}
				checkTelemetrySchemaAt(t, sb.Receiver.evidence(), "codex", "relay/codex-"+relayMode(content), newest)
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				if n := sum(c, "dropped.unclaimed"); n > 0 {
					t.Errorf("an opted-in session lost %d records waiting for its claim: %v", n, c)
				}
				if sum(c, "dropped.no_session_id.metrics") == 0 {
					Note(t.Name(), "Codex metrics arrived with no unattributable points: check whether a release added conversation.id")
				}
			})
		}
	})
}

// Nothing leaves for a session no opted-in repository claimed: one outside any
// repository, one in a repository without a binding, one in the installed repository
// on a machine that holds no key for its project.
func TestRelayNegativeControls(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		const hold = 3 * time.Second
		run := func(t *testing.T, sb *Sandbox) string {
			var calls atomic.Int32
			provider := httptest.NewServer(claudeTelemetryProvider(&calls))
			defer provider.Close()
			sb.ClaudeBaseURL = provider.URL
			_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
			return sid
		}
		expectNothing := func(t *testing.T, sb *Sandbox, reason string) {
			// Past the hold, and past the relay's one-second sweep.
			time.Sleep(hold + 3*time.Second)
			if n := agentRecords(sb.Receiver.evidence()); n != 0 {
				t.Errorf("%d agent records reached upstream for a session nobody opted in", n)
			}
			sb.StopRelay()
			c := sb.RelayStats()
			noteRelayStats(t.Name(), c)
			if sum(c, "received.") == 0 {
				t.Fatalf("the relay received nothing, so the control proves nothing: %v", c)
			}
			if sum(c, "forwarded.") != 0 || sum(c, "dropped."+reason) == 0 {
				t.Errorf("want everything dropped as %s: %v", reason, c)
			}
		}
		for _, where := range []string{"outside-repo", "unbound-repo"} {
			t.Run(where, func(t *testing.T) {
				track(t)
				t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
				sb := New(t, Isolated, WithClaude(b))
				sb.UseRelay(RelayOptions{Start: true, Hold: hold})
				dir := filepath.Join(sb.Dir, where)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if where == "unbound-repo" {
					sb.gitIn(dir, "init", "-q", "-b", "main")
				}
				sb.WorkDir = dir
				run(t, sb)
				expectNothing(t, sb, "unclaimed_expired")
			})
		}
		t.Run("no-key", func(t *testing.T) {
			track(t)
			t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
			sb := New(t, Isolated, WithClaude(b))
			sb.UseRelay(RelayOptions{Start: true, Hold: hold, NoKey: true})
			run(t, sb)
			expectNothing(t, sb, "no_key")
		})
	})
}

// The relay is not running when the agent starts: the first hook that claims the
// session starts it. This records what arrived rather than failing on it — how the
// first export races the relay's start is what the spike is here to measure.
func TestRelayColdStart(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		if !sb.waitRelay(10 * time.Second) {
			t.Fatal("no hook started the relay")
		}
		var failures contractFailures
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
			failures = nil
			checkClaudeTelemetry(&failures, sb.Receiver.evidence(), sid, sb.ProjectID, true)
			if len(failures) == 0 || time.Now().After(deadline) {
				break
			}
		}
		sb.StopRelay()
		noteRelayStats(t.Name(), sb.RelayStats())
		if len(failures) == 0 {
			Note(t.Name(), "cold start: the whole contract arrived")
		} else {
			Note(t.Name(), fmt.Sprintf("cold start: %d contract checks missed, first: %s", len(failures), failures[0]))
		}
	})
}

// A claim that arrives after the session's first exports — a Codex session whose
// hooks were trusted mid-way, a hook that timed out — releases what the relay held,
// as long as it lands inside the hold. The session runs where no hook can claim it,
// and the claim is written the way a hook writes it, seconds later.
func TestRelayLateClaim(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Hold: time.Minute})
		dir := filepath.Join(sb.Dir, "elsewhere")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.WorkDir = dir
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		time.Sleep(3 * time.Second)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Fatalf("%d records reached upstream before any claim", n)
		}
		claim := fmt.Sprintf(`{"project_id":%q,"tool":"claude-code","claimed_at":%q}`, sb.ProjectID, time.Now().UTC().Format(time.RFC3339Nano))
		sb.writeAbs(filepath.Join(sb.TermaConfig, "relay", "claims", sid+".json"), claim+"\n")
		deadline := time.Now().Add(30 * time.Second)
		for agentRecords(sb.Receiver.evidence()) == 0 && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		var failures contractFailures
		checkOnlyClaimed(&failures, sb.Receiver.evidence(), "session.id", sid, sb.ProjectID)
		for _, f := range failures {
			t.Error(f)
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		if c["released_after_hold"] == 0 || sum(c, "forwarded.") == 0 || sum(c, "dropped.unclaimed") != 0 {
			t.Errorf("want everything held and then released by the late claim: %v", c)
		}
	})
}
