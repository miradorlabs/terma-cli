package e2e

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// museRelaySandbox is a machine set up with Muse chosen, its relay started with content allowed
// or withheld, and Meta's copy of Muse's telemetry sent to a recording proxy that answers it
// locally and sends nothing on: what the relay passes Meta is seen, and never leaves.
func museRelaySandbox(t *testing.T, b Binary, content bool, steps func(sb *Sandbox) []museCall) (*Sandbox, *EgressProxy) {
	t.Helper()
	sb := New(t, Isolated)
	proxy := StartEgressProxy(t)
	var s []museCall
	if steps != nil {
		s = steps(sb)
	}
	provider := httptest.NewServer(museProvider(new(atomic.Int32), s))
	t.Cleanup(provider.Close)
	sb.ExtraEnv = append(sb.ExtraEnv, proxy.Env()...)
	sb.UseMuseSetup(b, provider.URL)
	sb.UseRelay(RelayOptions{Start: true, Content: content})
	return sb, proxy
}

// museSession is the root session Muse's hooks announced.
func museSession(t *testing.T, sb *Sandbox) string {
	t.Helper()
	starts := sb.WaitEvents("terma.session.start", "", 10*time.Second)
	if len(starts) == 0 {
		t.Fatalf("Muse announced no session; spool: %+v", sb.Spool())
	}
	return starts[0].SessionID
}

// metaCopies are the requests for Meta the relay passed on, by path.
func metaCopies(p *EgressProxy) map[string]int {
	out := map[string]int{}
	for _, r := range p.Requests() {
		if r.Host == "api.meta.ai" || strings.HasPrefix(r.Host, "api.meta.ai:") {
			out[r.Path]++
		}
	}
	return out
}

// Muse's session through the relay, content allowed and withheld: setup points Muse's own
// telemetry at the relay, its hooks claim the session, and the claimed session's logs and spans
// reach the project, placed by the root session, while Meta is still passed its copy of each.
func TestRelayMuse(t *testing.T) {
	forEachMuse(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		for _, content := range []bool{true, false} {
			t.Run(relayMode(content), func(t *testing.T) {
				Proves(t, b.Harness, b.Version, map[bool]string{true: "relay.content_allowed", false: "relay.content_withheld"}[content])
				track(t)
				sb, proxy := museRelaySandbox(t, b, content, func(sb *Sandbox) []museCall {
					return []museCall{museBash("printf ok"), {"muse.write_file", map[string]any{"path": filepath.Join(sb.Repo, "hello.txt"), "content": "TERMA_MUSE_FILE\n"}}}
				})
				sb.MuseRun(sb.Repo, "Write it. TERMA_MUSE_PROMPT")
				sid := museSession(t, sb)
				var e telemetryEvidence
				for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
					e = sb.Receiver.evidence()
					if museEvents(e)["turn_end"] > 0 || time.Now().After(deadline) {
						break
					}
				}
				time.Sleep(time.Second) // what the same export carried after it
				e = sb.Receiver.evidence()
				var failures contractFailures
				checkOnlyClaimed(&failures, e, "session_root_id", sid, sb.ProjectID)
				for _, f := range failures {
					t.Error(f)
				}
				events := museEvents(e)
				for _, want := range []string{"session_start", "turn_start", "model_call", "tool_call", "turn_end"} {
					if events[want] == 0 {
						t.Errorf("Muse's %s did not reach the project: %v, relay %v", want, events, sb.RelayStats())
					}
				}
				spans := map[string]int{}
				for _, s := range e.spans {
					spans[strings.Fields(s.Name)[0]]++
				}
				if spans["chat"] == 0 || spans["execute_tool"] == 0 {
					t.Errorf("Muse's spans did not reach the project: %v", spans)
				}
				if !content {
					if leaked := leakedFieldsOf(e, "TERMA_MUSE_PROMPT", "TERMA_MUSE_FILE"); len(leaked) > 0 {
						t.Errorf("content withheld, but it reached upstream in: %v", leaked)
					}
				}
				sb.StopRelay()
				c := sb.RelayStats()
				noteRelayStats(t.Name(), c)
				failUnclassified(t, c)
				if n := sum(c, "dropped."); n > 0 {
					t.Errorf("the relay dropped %d records of an opted-in session: %v", n, c)
				}
				copies := metaCopies(proxy)
				if copies["/muse-code/telemetry/logs"] == 0 || copies["/muse-code/telemetry/traces"] == 0 {
					t.Errorf("Meta was not passed its copy of Muse's telemetry: %v, relay %v", copies, c)
				}
			})
		}
	})
}

// Muse outside any admitted repository: its telemetry reaches the relay, which drops the
// unclaimed session, and nothing reaches the project; Meta is still passed its copy.
func TestRelayMuseOutsideARepository(t *testing.T) {
	forEachMuse(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb, proxy := museRelaySandbox(t, b, true, nil)
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.MuseRun(personal, "Reply with exactly: done. TERMA_PERSONAL_WORK")
		time.Sleep(8 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("Muse outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from Muse, so the control proves nothing: %v", c)
		}
		if copies := metaCopies(proxy); copies["/muse-code/telemetry/logs"] == 0 {
			t.Errorf("Meta was not passed its copy of an unclaimed session: %v", copies)
		}
	})
}

// museEvents counts Muse's own log events in e, by event_name.
func museEvents(e telemetryEvidence) map[string]int {
	out := map[string]int{}
	for _, r := range e.logs {
		if r.Resource["service.name"] == "tbh" {
			out[r.Attrs["event_name"]]++
		}
	}
	return out
}
