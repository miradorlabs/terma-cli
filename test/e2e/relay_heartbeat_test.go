package e2e

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The relay's heartbeat is a log record sent to the project's ingest with the project's
// key, every period, naming terma's version, the machine and the signed-in user.
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
		beats := sb.Receiver.WaitLogs(30*time.Second, func(l LogRecord) bool { return l.EventName == "terma.relay.heartbeat" })
		if len(beats) == 0 {
			t.Fatal("no heartbeat reached the project's ingest")
		}
		beat := beats[0]
		if beat.Resource["service.name"] != "terma-relay" || beat.Resource["service.version"] == "" || beat.Resource["terma.schema.version"] != "2" {
			t.Errorf("heartbeat resource %v", beat.Resource)
		}
		for _, k := range []string{"host.name", "terma.relay.heartbeat.reason"} {
			if beat.Attrs[k] == "" {
				t.Errorf("heartbeat has no %s: %v", k, beat.Attrs)
			}
		}
		if beat.Attrs["terma.user.id"] != accountUser {
			t.Errorf("heartbeat terma.user.id = %q, want the signed-in %s: %v", beat.Attrs["terma.user.id"], accountUser, beat.Attrs)
		}
		if _, ok := beat.Resource["terma.user.id"]; ok {
			t.Errorf("heartbeat carries terma.user.id on its resource: %v", beat.Resource)
		}
		for _, k := range []string{"event.name", "terma.version"} {
			if _, ok := beat.Attrs[k]; ok {
				t.Errorf("heartbeat still carries %s: %v", k, beat.Attrs)
			}
		}
	})
}
