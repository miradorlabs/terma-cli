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

func piSandbox(cmd string) func(t *testing.T) *Sandbox {
	if cmd == "" {
		return piToolSandbox("", nil)
	}
	return piToolSandbox("bash", map[string]any{"command": cmd})
}

// piToolSandbox is a bound repository whose Pi is asked for one call of tool.
func piToolSandbox(tool string, args map[string]any) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"pi"}
		sb.terma(sb.Repo, "install", "--project", sb.ProjectID, "--harness", "none", "--yes", "--no-browser", "--no-doctor")
		var calls atomic.Int32
		provider := httptest.NewServer(openAIToolCallProvider(&calls, tool, args))
		t.Cleanup(provider.Close)
		sb.UsePiProvider(provider.URL)
		return sb
	}
}

// Pi through the relay: terma's extension exports the same spans and logs as it does
// pointed straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsPi(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		for _, w := range []struct {
			name, tool string
			args       map[string]any
		}{
			{"reply", "", nil},
			{"bash", "bash", map[string]any{"command": "printf ok"}},
			{"write", "write", map[string]any{"path": "hello.txt", "content": "hello\n"}},
		} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, piToolSandbox(w.tool, w.args), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UsePiExtensionDirect()
					}
					sb.PiRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// Pi outside any bound repository: its extension exports to the relay, the relay
// holds the unclaimed session and drops it, and nothing reaches upstream.
func TestRelayPiOutsideARepository(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := piSandbox("")(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.PiRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("Pi outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from Pi, so the control proves nothing: %v", c)
		}
	})
}

// A file Pi writes is attributed: the extension's pi-file-edit lands in the session's
// manifest, and the commit of it is stamped with Pi's session.
func TestPiAttribution(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		track(t)
		sb := piToolSandbox("write", map[string]any{"path": "hello.txt", "content": "hello\n"})(t)
		sb.UsePiExtensionDirect()
		sb.PiRun(b, sb.Repo, "Write hello.txt.")
		starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
		if len(starts) == 0 {
			t.Fatalf("Pi announced no session; spool: %+v", sb.Spool())
		}
		sid := starts[0].SessionID
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v", sid, sb.Spool())
		} else if files, _ := evs[0].Attrs["files"].(string); !strings.Contains(files, "hello.txt") {
			t.Errorf("files touched = %q", files)
		}
		if n := len(sb.Events("terma.session.start", sid)); n != 1 {
			t.Errorf("%d terma.session.start for one Pi session, want 1 (a prompt claims, it does not announce)", n)
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: pi") {
			t.Errorf("commit not stamped with Pi's session %s:\n%s", sid, msg)
		}
	})
}

// Pi's session through the relay, content allowed and withheld: everything its
// extension exports reaches upstream under the project, and with content off neither
// the prompt, the reply nor the tool's command and output does.
func TestRelayPi(t *testing.T) {
	forEachPi(t, func(t *testing.T, b Binary) {
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				track(t)
				sb := piToolSandbox("bash", map[string]any{"command": "printf TERMA_PI_TOOL_OUTPUT"})(t)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sb.PiRun(b, sb.Repo, "Run it. TERMA_PI_PROMPT")
				starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
				if len(starts) == 0 {
					t.Fatalf("Pi announced no session; spool: %+v", sb.Spool())
				}
				sid := starts[0].SessionID
				deadline := time.Now().Add(20 * time.Second)
				for agentRecords(sb.Receiver.evidence()) == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				if agentRecords(e) == 0 {
					t.Fatalf("nothing of the opted-in Pi session reached upstream: %v", sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				checkExportRequests(&failures, e, "/v1/logs", "/v1/traces")
				for _, f := range failures {
					t.Error(f)
				}
				leaked := leakedFieldsOf(e, "TERMA_PI_PROMPT", "TERMA_TELEMETRY_REPLY", "TERMA_PI_TOOL_OUTPUT")
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
