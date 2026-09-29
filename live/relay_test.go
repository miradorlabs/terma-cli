package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
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
	// A record naming no session passes only as the relay's inference, said on its
	// resource: attributed by the process that sent it, to this session.
	inferred := func(res map[string]string) bool {
		return res["terma.relay.attribution"] == "process" && res["terma.relay.session.id"] == sid
	}
	for _, r := range e.logs {
		if !agent(r.Resource) {
			continue
		}
		if r.Attrs[key] == "" && inferred(r.Resource) && r.Resource["mirador.project.id"] == project {
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
		if a := s.Resource["terma.relay.attribution"]; a != "" && !inferred(s.Resource) {
			t.Errorf("span %s attributed by process to another session: %v", s.Name, s.Resource)
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
		if a := m.Resource["terma.relay.attribution"]; a != "" && !inferred(m.Resource) {
			t.Errorf("metric %s attributed by process to another session: %v", m.Proto.GetName(), m.Resource)
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
	// Totals per counter family — every drop reason by name, whatever it is.
	totals := map[string]int{}
	for k, v := range c {
		family := k
		if i := strings.LastIndexByte(k, '.'); i > 0 && (strings.HasSuffix(k, ".logs") || strings.HasSuffix(k, ".traces") || strings.HasSuffix(k, ".metrics")) {
			family = k[:i]
		}
		totals[family] += v
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, totals[k]))
	}
	Note(name, "relay: "+strings.Join(parts, " "))
}

// leakedFields names every span, span event and log field upstream received that
// carries one of the scenario's content markers: what to add to the relay's withheld
// fields when a harness release starts exporting content somewhere new.
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
				if sum(c, "attributed_by_process.metrics") == 0 {
					t.Errorf("no Codex metric was attributed by its process: %v", c)
				}
				if n := sum(c, "dropped.") - sum(c, "dropped.no_session_trace"); n > 0 {
					Note(t.Name(), fmt.Sprintf("dropped %d records other than unnamed traces", n))
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
		t.Run("hooks-off", func(t *testing.T) {
			// An opted-in repository with its key, but TERMA_HOOKS=0: no hook claims, so
			// nothing is forwarded — the kill switch holds for telemetry too.
			track(t)
			t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
			sb := New(t, Isolated, WithClaude(b))
			sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_HOOKS=0")
			sb.UseRelay(RelayOptions{Start: true, Hold: hold})
			run(t, sb)
			expectNothing(t, sb, "unclaimed_expired")
		})
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

// reached maps each session that reached upstream to the keys it was sent with and
// the projects on its resources.
func reached(r *Receiver) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	note := func(session, what string) {
		if session == "" {
			return
		}
		if out[session] == nil {
			out[session] = map[string]bool{}
		}
		out[session][what] = true
	}
	for _, req := range r.Requests() {
		switch m := req.Payload.(type) {
		case *collogspb.ExportLogsServiceRequest:
			for _, rl := range m.ResourceLogs {
				res := flatten(rl.GetResource().GetAttributes())
				if res["service.name"] == "terma-cli" {
					continue
				}
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						a := flatten(lr.Attributes)
						s := a["session.id"] + a["conversation.id"]
						note(s, "key "+req.Authorization)
						note(s, "project "+res["mirador.project.id"])
					}
				}
			}
		case *coltracepb.ExportTraceServiceRequest:
			for _, rs := range m.ResourceSpans {
				res := flatten(rs.GetResource().GetAttributes())
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						a := flatten(sp.Attributes)
						note(a["session.id"], "key "+req.Authorization)
						note(a["session.id"], "project "+res["mirador.project.id"])
					}
				}
			}
		}
	}
	return out
}

// Two opted-in projects and a personal session at once, through one relay: each
// project's session reaches only that project, with only that project's key, and the
// personal one reaches nothing.
func TestRelayConcurrentProjects(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Hold: 5 * time.Second})
		const otherProject, otherKey = "proj_other", "ter_srv_111111111111111111111111"
		other := filepath.Join(sb.Dir, "other")
		personal := filepath.Join(sb.Dir, "personal")
		for _, d := range []string{other, personal} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "live@terma.test"}, {"config", "user.name", "Terma Live"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
			sb.gitIn(other, args...)
		}
		sb.terma(other, "install", "--project", otherProject, "--harness", "none", "--adapters", "claude", "--yes", "--no-browser", "--no-doctor")
		keys, _ := json.Marshal(map[string]any{"keys": map[string]string{sb.ProjectID: liveKey, otherProject: otherKey}})
		sb.writeAbs(filepath.Join(sb.TermaConfig, "keys.json"), string(keys))

		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		sids := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, dir := range map[string]string{"live": sb.Repo, "other": other, "personal": personal} {
			wg.Go(func() {
				// One provider serves all three, and whichever call comes first is asked
				// to run the tool: every session gets the turns and the tool to do it.
				_, sid := sb.ClaudeHeadlessIn(dir, RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
				mu.Lock()
				sids[name] = sid
				mu.Unlock()
			})
		}
		wg.Wait()
		want := map[string]map[string]bool{
			sids["live"]:  {"key Bearer " + liveKey: true, "project " + sb.ProjectID: true},
			sids["other"]: {"key Bearer " + otherKey: true, "project " + otherProject: true},
		}
		deadline := time.Now().Add(45 * time.Second)
		var got map[string]map[string]bool
		for {
			got = reached(sb.Receiver)
			if len(got[sids["live"]]) > 0 && len(got[sids["other"]]) > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		time.Sleep(7 * time.Second) // past the hold: the personal session has had its chance to leak
		got = reached(sb.Receiver)
		for sid, facts := range want {
			if fmt.Sprint(got[sid]) != fmt.Sprint(facts) {
				t.Errorf("session %s reached upstream as %v, want %v", sid, got[sid], facts)
			}
		}
		if leaked := got[sids["personal"]]; len(leaked) > 0 {
			t.Errorf("the personal session reached upstream: %v", leaked)
		}
		sb.StopRelay()
		noteRelayStats(t.Name(), sb.RelayStats())
	})
}

// The relay started by a Codex hook: Codex runs the hook, so a relay it starts must
// still be able to listen and to reach upstream. Fails if no hook could start one;
// records how much of the contract arrived.
func TestRelayCodexColdStart(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{})
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		if !sb.waitRelay(10 * time.Second) {
			t.Fatal("no Codex hook started the relay")
		}
		var failures contractFailures
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
			failures = nil
			checkCodexTelemetry(&failures, sb.Receiver.evidence(), run.ThreadID, sb.ProjectID, true, true)
			if len(failures) == 0 || time.Now().After(deadline) {
				break
			}
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		if sum(c, "forwarded.") == 0 {
			t.Fatalf("the hook-started relay forwarded nothing: %v", c)
		}
		if len(failures) == 0 {
			Note(t.Name(), "cold start: the whole contract arrived")
		} else {
			Note(t.Name(), fmt.Sprintf("cold start: %d contract checks missed, first: %s", len(failures), failures[0]))
		}
	})
}

// fileSecret is written into a file by one tool and read back by another: the withheld
// scenario fails if it reaches upstream anywhere — an attribute, a span event, a body.
const fileSecret = "TERMA_FILE_SECRET_CONTENT"

// claudeScriptedProvider asks for each tool call in steps in turn, one per request,
// then replies with TERMA_TELEMETRY_REPLY.
func claudeScriptedProvider(calls *atomic.Int32, steps []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		call := int(calls.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", fmt.Sprintf("req_scripted_%d", call))
		emit := func(name string, payload any) {
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_scripted_%d", call), "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 0}}})
		stop := "end_turn"
		if call <= len(steps) {
			stop = "tool_use"
			step := steps[call-1]
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_scripted_%d", call), "name": step["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(step["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "TERMA_TELEMETRY_REPLY"}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		emit("message_stop", map[string]any{"type": "message_stop"})
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
				sb.UseRelay(RelayOptions{Start: true, Content: content})
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
				deadline := time.Now().Add(30 * time.Second)
				for len(sb.APIRequests(sid, time.Second)) < len(steps)+1 && time.Now().Before(deadline) {
					time.Sleep(time.Second)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				var found []string
				for _, f := range leakedFieldsOf(e, fileSecret) {
					found = append(found, f)
				}
				if content && len(found) == 0 {
					t.Errorf("tool content allowed, but the file's contents reached upstream nowhere")
				}
				if !content && len(found) > 0 {
					t.Errorf("tool content withheld, but the file's contents reached upstream in: %v", found)
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				for _, f := range failures {
					t.Error(f)
				}
				sb.StopRelay()
				noteRelayStats(t.Name(), sb.RelayStats())
				if content {
					Note(t.Name(), "file contents upstream in: "+strings.Join(found, ", "))
				}
			})
		}
	})
}

// resumeClaude resumes session sid headlessly in dir, the way a developer would with
// `claude --resume`, and returns the session id the resumed run reports.
func (sb *Sandbox) resumeClaude(dir, sid, prompt string) (string, error) {
	args := []string{"-p", prompt, "--output-format", "json", "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash",
		"--model", "claude-haiku-4-5", "--strict-mcp-config", "--no-chrome", "--disable-slash-commands", "--resume", sid}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.claudeLauncher(), args...)
	cmd.Dir = dir
	cmd.Env = sb.claudeEnv(RouteAPIKey)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(out, &result)
	return result.SessionID, nil
}

// A session claimed in an opted-in repository, resumed somewhere personal: if Claude
// keeps the session id, the claim (valid for hours) would forward the personal work.
// This records what happens rather than assuming it.
func TestRelayResumedElsewhere(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
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
		time.Sleep(8 * time.Second)
		leaked := leakedFieldsOf(sb.Receiver.evidence(), "TERMA_PERSONAL_WORK")
		records := 0
		for _, r := range sb.Receiver.evidence().logs {
			if r.Attrs["event.name"] == "user_prompt" && r.Attrs["session.id"] == resumed && r.Attrs["prompt_length"] == fmt.Sprint(len(personalPrompt)) {
				records++
			}
		}
		sb.StopRelay()
		noteRelayStats(t.Name(), sb.RelayStats())
		switch {
		case resumed != sid:
			Note(t.Name(), "resume elsewhere: Claude gave the resumed run a new session id, so the claim does not follow it")
		case records > 0 || len(leaked) > 0:
			t.Errorf("a session claimed in the repository and resumed in a personal directory was forwarded: prompt records %d, content in %v", records, leaked)
		default:
			Note(t.Name(), "resume elsewhere: same session id, nothing forwarded")
		}
	})
}
