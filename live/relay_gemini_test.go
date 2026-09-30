package live

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// geminiSandbox is a bound repository whose Gemini is asked for one call of tool.
func geminiSandbox(tool string, args func(sb *Sandbox) map[string]any) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"gemini"}
		sb.terma(sb.Repo, "install", "--project", sb.ProjectID, "--harness", "none", "--yes", "--no-browser", "--no-doctor")
		var a map[string]any
		if args != nil {
			a = args(sb)
		}
		provider := httptest.NewServer(geminiProvider(new(atomic.Int32), tool, a))
		t.Cleanup(provider.Close)
		sb.UseGemini(provider.URL, filepath.Join(sb.Dir, "personal"))
		return sb
	}
}

func geminiWrite(content string) func(sb *Sandbox) map[string]any {
	return func(sb *Sandbox) map[string]any {
		return map[string]any{"file_path": filepath.Join(sb.Repo, "hello.txt"), "content": content}
	}
}

// Gemini CLI through the relay: its native export, with the relay's token in the
// endpoint's path, arrives the same as pointed straight at the receiver, nothing
// dropped.
func TestRelayWorkloadsGemini(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		for _, w := range []struct {
			name, tool string
			args       func(sb *Sandbox) map[string]any
		}{
			{"reply", "", nil},
			{"write", "write_file", geminiWrite("hello\n")},
			{"shell", "run_shell_command", func(*Sandbox) map[string]any { return map[string]any{"command": "printf ok"} }},
		} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, geminiSandbox(w.tool, w.args), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UseGeminiDirect()
					}
					sb.GeminiRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// Gemini's session through the relay, content allowed and withheld: with content off,
// neither the prompt, the reply, the file's content nor the command line that carries
// the prompt (process.command_args) reaches upstream.
func TestRelayGemini(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				sb := geminiSandbox("write_file", geminiWrite("TERMA_GEMINI_FILE\n"))(t)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sid := sb.GeminiRun(b, sb.Repo, "Write it. TERMA_GEMINI_PROMPT")
				if sid == "" {
					t.Fatal("Gemini reported no session")
				}
				deadline := time.Now().Add(20 * time.Second)
				for agentRecords(sb.Receiver.evidence()) == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				if agentRecords(e) == 0 {
					t.Fatalf("nothing of the opted-in Gemini session reached upstream: %v", sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				for _, f := range failures {
					t.Error(f)
				}
				leaked := leakedFieldsOf(e, "TERMA_GEMINI_PROMPT", "TERMA_TELEMETRY_REPLY", "TERMA_GEMINI_FILE")
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

// Gemini outside any bound repository: it exports to the relay, the relay holds the
// unclaimed session and drops it, and nothing reaches upstream.
func TestRelayGeminiOutsideARepository(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		track(t)
		sb := geminiSandbox("", nil)(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.GeminiRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("Gemini outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from Gemini, so the control proves nothing: %v", c)
		}
	})
}

// Nothing Gemini runs inherits the relay's endpoint or token: the settings file carries
// the endpoint, the token rides its path, and no environment is set.
func TestRelayGeminiToolsGetNoExporter(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		track(t)
		out := filepath.Join(os.TempDir(), "terma-gemini-env-"+time.Now().Format("150405.000000"))
		t.Cleanup(func() { _ = os.Remove(out) })
		sb := geminiSandbox("run_shell_command", func(*Sandbox) map[string]any {
			return map[string]any{"command": "env | cut -d= -f1 | grep '^OTEL_' > " + out + "; printf ran >> " + out}
		})(t)
		sb.UseRelay(RelayOptions{Start: true})
		sb.GeminiRun(b, sb.Repo, "Run it.")
		data, _ := os.ReadFile(out)
		if !strings.Contains(string(data), "ran") {
			t.Fatalf("Gemini's shell tool never ran: %q", data)
		}
		if names := strings.Fields(strings.ReplaceAll(string(data), "ran", "")); len(names) > 0 {
			t.Errorf("Gemini's tools inherited exporter settings: %v", names)
		}
		sb.StopRelay()
	})
}

// A file Gemini writes is attributed: terma's extension's AfterTool hook lands it in the
// session's manifest, and the commit of it is stamped with Gemini's session.
func TestGeminiAttribution(t *testing.T) {
	forEachGemini(t, func(t *testing.T, b Binary) {
		track(t)
		sb := geminiSandbox("write_file", geminiWrite("hello\n"))(t)
		sb.UseGeminiDirect()
		sid := sb.GeminiRun(b, sb.Repo, "Write hello.txt.")
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v", sid, sb.Spool())
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: gemini") {
			t.Errorf("commit not stamped with Gemini's session %s:\n%s", sid, msg)
		}
	})
}
