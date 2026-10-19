package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilotSandbox is an admitted repository on a machine set up with Copilot chosen.
func copilotSandbox(t *testing.T, b Binary) *Sandbox {
	sb := New(t, Isolated)
	sb.UseCopilot(b)
	return sb
}

// Copilot CLI's session through the relay, content allowed and withheld: started through
// `terma launch` its exporter reaches the relay, its hooks claim the session, and with
// content off neither the prompt nor the file it wrote reaches upstream.
func TestRelayCopilot(t *testing.T) {
	forEachCopilot(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[content])
				track(t)
				sb := copilotSandbox(t, b)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sid, out := sb.CopilotRun(sb.Repo, "Create a file named hello.txt containing exactly TERMA_COPILOT_FILE, then reply with exactly: done. TERMA_COPILOT_PROMPT")
				if sid == "" {
					t.Fatalf("Copilot reported no session:\n%s", tail(out, 2000))
				}
				// The CLI's metrics name no session: the relay places them by the process that
				// sent them once it has exited, a grace period after its last export.
				deadline := time.Now().Add(45 * time.Second)
				for sb.RelayStats()["forwarded.metrics"] == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				if len(e.spans) == 0 || len(e.metrics) == 0 {
					t.Fatalf("the opted-in Copilot session reached upstream with %d spans and %d metrics: %v", len(e.spans), len(e.metrics), sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "gen_ai.conversation.id", sid, sb.ProjectID)
				for _, f := range failures {
					t.Error(f)
				}
				// The turn arrived whole: its agent span, a request with its token counts, and the edit.
				names := map[string]bool{}
				tokens := false
				for _, s := range e.spans {
					if v := s.Attrs["gen_ai.conversation.id"]; v != sid {
						t.Errorf("relay forwarded span %s of another session: gen_ai.conversation.id=%q", s.Name, v)
					}
					names[strings.Fields(s.Name)[0]] = true
					tokens = tokens || s.Attrs["gen_ai.usage.input_tokens"] != "" && s.Attrs["gen_ai.usage.output_tokens"] != ""
				}
				if !names["invoke_agent"] || !names["chat"] || !names["execute_tool"] || !tokens {
					t.Errorf("the session arrived without its agent span, a request's token counts or a tool call: %v, tokens %v", names, tokens)
				}
				leaked := leakedFieldsOf(e, "TERMA_COPILOT_PROMPT", "TERMA_COPILOT_FILE")
				if content && len(leaked) == 0 {
					t.Errorf("content allowed, but none reached upstream")
				}
				if !content && len(leaked) > 0 {
					t.Errorf("content withheld, but it reached upstream in: %v", leaked)
				}
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				failUnclassified(t, c)
				if n := sum(c, "dropped."); n > 0 {
					t.Errorf("the relay dropped %d records of an opted-in session: %v", n, c)
				}
			})
		}
	})
}

// Copilot outside any admitted repository: it exports to the relay, the relay holds the
// unclaimed session and drops it, and nothing reaches upstream.
func TestRelayCopilotOutsideARepository(t *testing.T) {
	forEachCopilot(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb := copilotSandbox(t, b)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.CopilotRun(personal, "Reply with exactly: done. TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("Copilot outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from Copilot, so the control proves nothing: %v", c)
		}
	})
}

// Nothing Copilot runs inherits the relay's endpoint or token: `terma launch` puts them
// in the CLI's environment, and the CLI keeps every exporter variable out of its tools':
// none named OTEL_* or COPILOT_OTEL*, and none whose value names the relay's address.
func TestRelayCopilotToolsGetNoExporter(t *testing.T) {
	forEachCopilot(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.tools_no_token")
		track(t)
		out := filepath.Join(os.TempDir(), "terma-copilot-env-"+time.Now().Format("150405.000000"))
		t.Cleanup(func() { _ = os.Remove(out) })
		sb := copilotSandbox(t, b)
		sb.UseRelay(RelayOptions{Start: true})
		sb.CopilotRun(sb.Repo, "Run exactly this one shell command, then reply with exactly: done.\nenv | cut -d= -f1 | grep -E '^(OTEL_|COPILOT_OTEL)' > "+out+"; env | grep -c "+sb.relayAddr+" >> "+out+"; printf ran >> "+out)
		data, _ := os.ReadFile(out)
		if !strings.Contains(string(data), "ran") {
			t.Fatalf("Copilot's shell tool never ran: %q", data)
		}
		// The exporter variables' names, then how many variables name the relay's address.
		if seen := strings.Fields(strings.ReplaceAll(string(data), "ran", "")); len(seen) != 1 || seen[0] != "0" {
			t.Errorf("Copilot's tools inherited exporter settings: %q", data)
		}
		sb.StopRelay()
	})
}

// A file Copilot writes is attributed: its PostToolUse hook lands it in the session's
// manifest, and the commit of it is stamped with Copilot's session. The prompt names the
// edit tool: a file a shell command writes is not attributed (README, Copilot), and
// which tool a model picks unprompted depends on the model the token is given.
func TestCopilotAttribution(t *testing.T) {
	forEachCopilot(t, func(t *testing.T, b Binary) {
		track(t)
		sb := copilotSandbox(t, b)
		sid, out := sb.CopilotRun(sb.Repo, "Using your file editing tool, not a shell command, create a file named hello.txt containing exactly hello, then reply with exactly: done.")
		if sid == "" {
			t.Fatalf("Copilot reported no session:\n%s", tail(out, 2000))
		}
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v; the session ran %s", sid, sb.Spool(), sb.CopilotTools(sid))
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: copilot") {
			t.Errorf("commit not stamped with Copilot's session %s:\n%s", sid, msg)
		}
	})
}
