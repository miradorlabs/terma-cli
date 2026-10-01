package e2e

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The relay's heartbeat goes to the organization the developer signed in to, with their
// credential, every period: after a real session through the relay it names terma's
// version, the agent build it saw and its counters — and no project. Nothing of it
// reaches the project's ingest.
func TestRelayHeartbeat(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.heartbeat")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		acct := sb.StartAccount()
		sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_RELAY_HEARTBEAT=3s")
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(claudeTelemetryProvider(&calls))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
		var attrs map[string]string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			for _, beat := range acct.Heartbeats() {
				for _, rl := range beat.ResourceLogs {
					if res := flatten(rl.Resource.GetAttributes()); res["mirador.project.id"] != "" || res["service.name"] != "terma-relay" {
						t.Fatalf("heartbeat resource %v", res)
					}
					for _, sl := range rl.ScopeLogs {
						for _, rec := range sl.LogRecords {
							if a := flatten(rec.Attributes); a["relay.agent.claude-code.version"] != "" {
								attrs = a
							}
						}
					}
				}
			}
			if attrs != nil {
				break
			}
		}
		if attrs == nil {
			t.Fatalf("no heartbeat naming the Claude build reached the account (%d beats)", len(acct.Heartbeats()))
		}
		for _, k := range []string{"event.name", "terma.version", "terma.os", "terma.machine_id", "terma.mode", "terma.organization_id", "relay.count.forwarded.logs", "relay.last_delivery_at"} {
			if attrs[k] == "" {
				t.Errorf("heartbeat has no %s: %v", k, attrs)
			}
		}
		if n := len(sb.Receiver.WaitLogs(time.Second, func(l LogRecord) bool { return l.Attrs["event.name"] == "terma.relay.heartbeat" })); n > 0 {
			t.Errorf("%d heartbeats reached the project's ingest", n)
		}
	})
}
