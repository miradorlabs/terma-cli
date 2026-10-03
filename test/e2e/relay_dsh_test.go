package e2e

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

// dshSandbox is an admitted repository whose dsh is asked for the given tool calls.
func dshSandbox(steps func(sb *Sandbox) []claudeStep) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"dsh"}
		var s []claudeStep
		if steps != nil {
			s = steps(sb)
		}
		provider := httptest.NewServer(claudeWorkloadProvider(new(atomic.Int32), s, false))
		t.Cleanup(provider.Close)
		sb.UseDsh(provider.URL)
		return sb
	}
}

func dshWrite(content string) func(sb *Sandbox) []claudeStep {
	return func(sb *Sandbox) []claudeStep {
		return []claudeStep{{{"name": "write", "input": map[string]any{"file_path": filepath.Join(sb.Repo, "hello.txt"), "content": content}}}}
	}
}

// dshSession is the session dsh announced through terma's plugin.
func dshSession(t *testing.T, sb *Sandbox) string {
	t.Helper()
	starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
	if len(starts) == 0 {
		t.Fatalf("dsh announced no session; spool: %+v", sb.Spool())
	}
	return starts[0].SessionID
}

// dsh through the relay: terma's plugin exports the same spans and logs as it does
// pointed straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsDsh(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.equivalent")
		for _, w := range []struct {
			name  string
			steps func(sb *Sandbox) []claudeStep
		}{
			{"reply", nil},
			{"write", dshWrite("hello\n")},
			{"bash", func(*Sandbox) []claudeStep { return []claudeStep{{bash("printf ok")}} }},
		} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, dshSandbox(w.steps), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UseDshDirect()
					}
					sb.DshRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// dsh's session through the relay, content allowed and withheld.
func TestRelayDsh(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[content])
				track(t)
				sb := dshSandbox(dshWrite("TERMA_DSH_FILE\n"))(t)
				sb.UseRelay(RelayOptions{Start: true, Content: content})
				sb.DshRun(b, sb.Repo, "Write it. TERMA_DSH_PROMPT")
				sid := dshSession(t, sb)
				deadline := time.Now().Add(20 * time.Second)
				for agentRecords(sb.Receiver.evidence()) == 0 && time.Now().Before(deadline) {
					time.Sleep(500 * time.Millisecond)
				}
				// The plugin exports on turn end and on exit: wait for the tool's span, not a
				// fixed time — on a loaded CI runner three seconds was once not enough.
				var e telemetryEvidence
				for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(250 * time.Millisecond) {
					e = sb.Receiver.evidence()
					seen := false
					for _, s := range e.spans {
						seen = seen || s.Name == "execute_tool write"
					}
					if seen || time.Now().After(deadline) {
						break
					}
				}
				time.Sleep(time.Second) // what the same flush carried after it
				e = sb.Receiver.evidence()
				if agentRecords(e) == 0 {
					t.Fatalf("nothing of the opted-in dsh session reached upstream: %v", sb.RelayStats())
				}
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session.id", sid, sb.ProjectID)
				for _, f := range failures {
					t.Error(f)
				}
				chats, tools := 0, 0
				for _, s := range e.spans {
					switch {
					case strings.HasPrefix(s.Name, "chat ") && s.Attrs["gen_ai.usage.output_tokens"] != "":
						chats++
					case s.Name == "execute_tool write":
						tools++
					}
				}
				aux := 0
				for _, s := range e.spans {
					if s.Attrs["dsh.purpose"] != "" {
						aux++
					}
				}
				Note(t.Name(), fmt.Sprintf("auxiliary model calls spanned: %d", aux))
				if chats == 0 || tools == 0 {
					t.Errorf("want chat spans with usage and the write tool span; got %d and %d", chats, tools)
				}
				leaked := leakedFieldsOf(e, "TERMA_DSH_PROMPT", "TERMA_TELEMETRY_REPLY", "TERMA_DSH_FILE")
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

// dsh outside any admitted folder reaches nothing upstream.
func TestRelayDshOutsideARepository(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb := dshSandbox(nil)(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.DshRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("dsh outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from dsh, so the control proves nothing: %v", c)
		}
	})
}

// A file dsh writes is attributed: the commit of it is stamped with dsh's session.
func TestDshAttribution(t *testing.T) {
	forEachDsh(t, func(t *testing.T, b Binary) {
		track(t)
		sb := dshSandbox(dshWrite("hello\n"))(t)
		sb.UseDshDirect()
		sb.DshRun(b, sb.Repo, "Write hello.txt.")
		sid := dshSession(t, sb)
		if evs := sb.WaitEvents("terma.files.touched", sid, 10*time.Second); len(evs) == 0 {
			t.Fatalf("no terma.files.touched for %s; spool: %+v", sid, sb.Spool())
		}
		msg := sb.Commit("add hello")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) || !strings.Contains(msg, "Agent-Tool: dsh") {
			t.Errorf("commit not stamped with dsh's session %s:\n%s", sid, msg)
		}
	})
}
