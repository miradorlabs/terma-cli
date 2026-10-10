package e2e

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"
)

// An interactive Claude session's fields join the census: some events exist only there, the
// status line's and permission_mode_changed among them, which says the permission modes a
// session moved between (from_mode, to_mode) and why (trigger). Headless workloads never
// emit it. A build that does not either is noted, not failed: it depends on the build and
// the route; the catalog says when it stops.
func TestClaudeInteractiveFields(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		Proves(t, b.Harness, b.Version, CensusCapability)
		const key = "synthetic-telemetry-key"
		t.Setenv("ANTHROPIC_API_KEY", key)
		sb := New(t, Isolated, WithClaude(b))
		// The key is approved already, as it is for a developer who has used it.
		state := map[string]any{}
		raw, _ := os.ReadFile(filepath.Join(sb.ClaudeConfig, ".claude.json"))
		_ = json.Unmarshal(raw, &state)
		state["customApiKeyResponses"] = map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}}
		raw, _ = json.MarshalIndent(state, "", "  ")
		sb.writeAbs(filepath.Join(sb.ClaudeConfig, ".claude.json"), string(raw))
		var calls atomic.Int32
		provider := httptest.NewServer(claudeScriptedProvider(&calls, []map[string]any{{"name": "Bash", "input": map[string]any{"command": "printf ok"}}}))
		defer provider.Close()
		sb.ClaudeBaseURL = provider.URL
		run := sb.ClaudeInteractive(RouteAPIKey, "turn one", regexp.MustCompile(`TERMA_TELEMETRY_REPLY`), "--tools", "Bash", "--allowedTools", "Bash")
		time.Sleep(6 * time.Second) // the exporter's last batch
		ObserveFields(b, sb.Receiver.Requests())
		changed := false
		for _, r := range sb.Receiver.Logs() {
			if r.Attrs["event.name"] != "permission_mode_changed" || r.Attrs["session.id"] != run.SessionID {
				continue
			}
			changed = true
			for _, k := range []string{"from_mode", "to_mode", "trigger"} {
				if r.Attrs[k] == "" {
					t.Errorf("permission_mode_changed without %s: %v", k, r.Attrs)
				}
			}
		}
		if !changed {
			Note(t.Name(), "no permission_mode_changed this session")
		}
	})
}
