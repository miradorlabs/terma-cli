package live

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func ompSandbox(t *testing.T, cmd string) func(t *testing.T) *Sandbox {
	return func(t *testing.T) *Sandbox {
		sb := New(t, Isolated)
		sb.RelayAgents = []string{"omp"}
		sb.terma(sb.Repo, "install", "--project", sb.ProjectID, "--harness", "none", "--adapters", "omp", "--yes", "--no-browser", "--no-doctor")
		var calls atomic.Int32
		provider := httptest.NewServer(openAIToolProvider(&calls, cmd))
		t.Cleanup(provider.Close)
		sb.UseOmpProvider(provider.URL)
		return sb
	}
}

// omp through the relay: the same telemetry as a direct export, nothing dropped —
// its native spans (gen_ai.conversation.id), its metrics (by process) and its logs.
func TestRelayWorkloadsOmp(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
		for _, w := range []struct{ name, cmd string }{{"reply", ""}, {"bash", "printf ok"}} {
			t.Run(w.name, func(t *testing.T) {
				runBoth(t, ompSandbox(t, w.cmd), func(t *testing.T, sb *Sandbox) {
					sb.OmpRun(b, sb.Repo, "Do the task. TERMA_WORKLOAD")
				})
			})
		}
	})
}

// omp outside any repository exports nothing at all: its shim hands it the relay's
// variables only in a bound repository.
func TestRelayOmpOutsideARepository(t *testing.T) {
	forEachOmp(t, func(t *testing.T, b Binary) {
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
		if n := agentRecords(sb.Receiver.evidence()); n != 0 || sum(c, "received.") != 0 {
			t.Errorf("omp outside a repository exported: %d upstream, relay %v", n, c)
		}
	})
}
