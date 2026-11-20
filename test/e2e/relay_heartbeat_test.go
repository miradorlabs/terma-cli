package e2e

import (
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// The relay's heartbeat is a log record sent to the selected team's ingest with the
// person's sign-in, naming the team, every period, naming terma's version, the machine,
// this installation and the signed-in user. No older terma's sign-in was retired here,
// so it names no legacy sessions.
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
		if beat.Authorization != personBearer() || beat.Project != sb.ProjectID {
			t.Errorf("heartbeat sent with %q naming team %q, want the person's sign-in naming %s", beat.Authorization, beat.Project, sb.ProjectID)
		}
		if beat.Resource["terma.installation.id"] != accountInstallation {
			t.Errorf("heartbeat resource terma.installation.id = %q, want %s: %v", beat.Resource["terma.installation.id"], accountInstallation, beat.Resource)
		}
		if _, ok := beat.Resource["terma.legacy_session_ids"]; ok {
			t.Errorf("heartbeat names legacy sessions no sign-in retired: %v", beat.Resource)
		}
		if beat.Resource["service.name"] != "terma-relay" || beat.Resource["service.version"] == "" || beat.Resource["terma.schema.version"] != "2" {
			t.Errorf("heartbeat resource %v", beat.Resource)
		}
		for _, k := range []string{"host.name", "terma.relay.heartbeat.reason"} {
			if beat.Attrs[k] == "" {
				t.Errorf("heartbeat has no %s: %v", k, beat.Attrs)
			}
		}
		// How terma was installed is the resource's, read as the relay started; whether it
		// updates itself is each record's.
		if !slices.Contains([]string{"script", "homebrew", "npm", "npm_project", "source"}, beat.Resource["terma.install.method"]) {
			t.Errorf("heartbeat resource terma.install.method = %q: %v", beat.Resource["terma.install.method"], beat.Resource)
		}
		if !slices.Contains([]string{"on", "off", "unsupported"}, beat.Attrs["terma.update.auto"]) {
			t.Errorf("heartbeat terma.update.auto = %q: %v", beat.Attrs["terma.update.auto"], beat.Attrs)
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
