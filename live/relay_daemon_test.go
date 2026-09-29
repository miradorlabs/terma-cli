package live

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func (sb *Sandbox) relayPID() int {
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// The relay as a launchd service (terma relay daemon install), for real: the
// service manager keeps it up — a killed relay is back without any hook — and it is
// already listening when an agent starts, so Codex's first event, which precedes every
// hook and is never retried, arrives too. Removing the service stops it.
func TestRelayDaemon(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the service test drives launchd")
	}
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Content: true})
		sb.terma(sb.Repo, "relay", "daemon", "install")
		t.Cleanup(func() { sb.terma(sb.Repo, "relay", "daemon", "remove") })
		if !sb.waitRelay(20 * time.Second) {
			t.Fatal("the service never started the relay")
		}
		first := sb.relayPID()
		if first == 0 {
			t.Fatal("no relay pid")
		}
		_ = syscall.Kill(first, syscall.SIGKILL)
		deadline := time.Now().Add(30 * time.Second)
		for (sb.relayPID() == first || !sb.waitRelay(time.Second)) && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		if sb.relayPID() == first {
			t.Fatal("launchd did not restart a killed relay")
		}
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
			checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, false, true)
			checkOnlyClaimed(r, e, "conversation.id", run.ThreadID, sb.ProjectID)
		})
		noteRelayStats(t.Name(), sb.RelayStats())
		sb.terma(sb.Repo, "relay", "daemon", "remove")
		for deadline := time.Now().Add(15 * time.Second); sb.waitRelay(200*time.Millisecond) && time.Now().Before(deadline); {
			time.Sleep(300 * time.Millisecond)
		}
		if sb.waitRelay(time.Second) {
			t.Fatal("the relay kept running after the service was removed")
		}
	})
}
