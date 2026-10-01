package e2e

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The relay's heartbeat is a log record sent to the project's ingest with the project's
// key, every period, naming terma's version and the machine.
func TestRelayHeartbeat(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.heartbeat")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		sb.StartAccount()
		sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_RELAY_HEARTBEAT=3s")
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		beats := sb.Receiver.WaitLogs(30*time.Second, func(l LogRecord) bool { return l.Attrs["event.name"] == "terma.relay.heartbeat" })
		if len(beats) == 0 {
			t.Fatal("no heartbeat reached the project's ingest")
		}
		beat := beats[0]
		if beat.Resource["service.name"] != "terma-relay" {
			t.Errorf("heartbeat resource %v", beat.Resource)
		}
		for _, k := range []string{"terma.version", "host.name", "terma.heartbeat.reason"} {
			if beat.Attrs[k] == "" {
				t.Errorf("heartbeat has no %s: %v", k, beat.Attrs)
			}
		}
	})
}
