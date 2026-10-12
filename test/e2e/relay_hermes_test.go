package e2e

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hermesSandbox is an admitted repository whose Hermes is asked for one call of tool.
func hermesSandbox(tool string, args map[string]any) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"hermes"}
		var calls atomic.Int32
		provider := httptest.NewServer(hermesProvider(&calls, tool, args))
		t.Cleanup(provider.Close)
		sb.UseHermes(provider.URL)
		return sb
	}
}

// hermesSession is the session Hermes announced through terma's plugin.
func hermesSession(t *testing.T, sb *Sandbox) string {
	t.Helper()
	starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
	if len(starts) == 0 {
		t.Fatalf("Hermes announced no session; spool: %+v", sb.Spool())
	}
	return starts[0].SessionID
}

// Hermes through the relay: terma's plugin exports the same spans and logs as it does
// pointed straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsHermes(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.equivalent")
		for _, w := range []struct {
			name, tool string
			args       map[string]any
		}{
			{"reply", "", nil},
			{"write", "write_file", map[string]any{"path": "hello.txt", "content": "hello\n"}},
		} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, b, hermesSandbox(w.tool, w.args), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UseHermesPluginDirect()
					}
					sb.HermesRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// Hermes's session through the relay, content allowed and withheld: everything the
// plugin exports reaches upstream under the project, and with content off neither the
// prompt, the reply nor the tool's arguments and result do.
func TestRelayHermes(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[content])
				track(t)
				sb := hermesSandbox("write_file", map[string]any{"path": "hello.txt", "content": "TERMA_HERMES_FILE\n"})(t)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sb.HermesRun(b, sb.Repo, "Write it. TERMA_HERMES_PROMPT")
				sid := hermesSession(t, sb)
				deadline := time.Now().Add(20 * time.Second)
				for agentRecords(sb.Receiver.evidence()) == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				time.Sleep(3 * time.Second)
				e := sb.Receiver.evidence()
				if agentRecords(e) == 0 {
					t.Fatalf("nothing of the opted-in Hermes session reached upstream: %v", sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				checkExportRequests(&failures, e, "/v1/logs", "/v1/traces")
				for _, f := range failures {
					t.Error(f)
				}
				chats, tools := 0, 0
				for _, s := range e.spans {
					switch {
					case strings.HasPrefix(s.Name, "chat ") && s.Attrs["gen_ai.usage.input_tokens"] != "":
						chats++
					case s.Name == "execute_tool write_file":
						tools++
					}
				}
				if chats == 0 || tools == 0 {
					t.Errorf("want chat spans with usage and the write_file tool span; got %d and %d", chats, tools)
				}
				leaked := leakedFieldsOf(e, "TERMA_HERMES_PROMPT", "TERMA_TELEMETRY_REPLY", "TERMA_HERMES_FILE")
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

// Hermes outside any admitted repository: its plugin exports to the relay, the relay holds
// the unclaimed session and drops it, and nothing reaches upstream.
func TestRelayHermesOutsideARepository(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb := hermesSandbox("", nil)(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.HermesRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("Hermes outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from Hermes, so the control proves nothing: %v", c)
		}
	})
}

// A file Hermes writes is attributed: the plugin's hermes-file-edit lands in the
// session's manifest, and the commit of it is stamped with Hermes's session.
func TestHermesAttribution(t *testing.T) {
	forEachHermes(t, func(t *testing.T, b Binary) {
		track(t)
		sb := hermesSandbox("write_file", map[string]any{"path": "hello.txt", "content": "hello\n"})(t)
		sb.UseHermesPluginDirect()
		sb.HermesRun(b, sb.Repo, "Write hello.txt.")
		sid := hermesSession(t, sb)
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v", sid, sb.Spool())
		} else if files := listAttr(evs[0].Attrs["terma.files.paths"]); !slices.Contains(files, "hello.txt") {
			t.Errorf("files touched = %q", files)
		}
		if n := len(sb.Events("terma.session.start", sid)); n != 1 {
			t.Errorf("%d terma.session.start for one Hermes session, want 1 (a prompt claims, it does not announce)", n)
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: hermes") {
			t.Errorf("commit not stamped with Hermes's session %s:\n%s", sid, msg)
		}
	})
}
