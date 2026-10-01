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

func ompSandbox(t *testing.T, cmd string) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"omp"}
		sb.terma(sb.Repo, "install", "--team", sb.ProjectID, "--harness", "none", "--adapters", "omp", "--yes", "--no-browser", "--no-doctor")
		var calls atomic.Int32
		provider := httptest.NewServer(openAIToolProvider(&calls, cmd))
		t.Cleanup(provider.Close)
		sb.UseOmpProvider(provider.URL)
		return sb
	}
}

// omp through the relay: terma's extension exports the same telemetry as it does
// pointed straight at the receiver, and the relay drops none of an opted-in session's.
func TestRelayWorkloadsOmp(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.equivalent")
		for _, w := range []struct{ name, cmd string }{{"reply", ""}, {"bash", "printf ok"}} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, ompSandbox(t, w.cmd), func(t *testing.T, sb *Sandbox) {
					if !sb.relayed {
						sb.UseOmpExtensionDirect()
					}
					sb.OmpRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// omp outside any bound repository: its extension exports to the relay, the relay holds
// the unclaimed session and drops it, and nothing reaches upstream.
func TestRelayOmpOutsideARepository(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.only_opted_in")
		track(t)
		sb := ompSandbox(t, "")(t)
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		sb.OmpRun(b, personal, "TERMA_PERSONAL_WORK")
		time.Sleep(6 * time.Second)
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if n := agentRecords(sb.Receiver.evidence()); n != 0 {
			t.Errorf("omp outside a repository reached upstream: %d records, relay %v", n, c)
		}
		if sum(c, "received.") == 0 {
			t.Errorf("the relay received nothing from omp, so the control proves nothing: %v", c)
		}
	})
}

// Nothing omp runs inherits terma's endpoint or its key: terma sets no environment for
// omp at all (see TestRelayClaudeToolsGetNoExporter for why it matters).
func TestRelayOmpToolsGetNoExporter(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.tools_no_token")
		track(t)
		out, err := os.CreateTemp("", "terma-omp-env-*")
		if err != nil {
			t.Fatal(err)
		}
		_ = out.Close()
		t.Cleanup(func() { _ = os.Remove(out.Name()) })
		sb := ompSandbox(t, "env | cut -d= -f1 | grep '^OTEL_' > "+out.Name()+"; printf ran >> "+out.Name())(t)
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		sb.OmpRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
		time.Sleep(6 * time.Second)
		data, _ := os.ReadFile(out.Name())
		if !strings.Contains(string(data), "ran") {
			t.Fatalf("omp's bash tool never ran: %q", data)
		}
		if names := strings.Fields(strings.ReplaceAll(string(data), "ran", "")); len(names) > 0 {
			t.Errorf("omp's tools inherited exporter settings: %v", names)
		}
		if agentRecords(sb.Receiver.evidence()) == 0 {
			t.Errorf("omp's session reached nothing upstream: %v", sb.RelayStats())
		}
		sb.StopRelay()
	})
}
