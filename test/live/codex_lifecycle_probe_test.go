package live

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// This opt-in diagnostic removes Terma's hook handlers from the equation.
// A shell builtin writes each marker; no spool, receiver or child CLI is involved.
func TestCodexSessionEndProbe(t *testing.T) {
	if os.Getenv("TERMA_LIVE_CODEX_LIFECYCLE_PROBE") != "1" {
		t.Skip("set TERMA_LIVE_CODEX_LIFECYCLE_PROBE=1 to investigate native shutdown")
	}
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.prepareCodex(RouteAPIKey)
		sb.write(".codex/hooks.json", `{"hooks":{
			"SessionStart":[{"hooks":[{"type":"command","command":"printf started > .codex/probe-start","timeout":3}]}],
			"SessionEnd":[{"hooks":[{"type":"command","command":"printf ended > .codex/probe-end","timeout":3}]}]
		}}`)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeCodexResponse(w, 1, map[string]any{"type": "message", "id": "msg_probe", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TERMA_TELEMETRY_REPLY", "annotations": []any{}}}})
		}))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, "Reply briefly.", fixtureCodexArgs(provider.URL)...)
		if !fileExists(filepath.Join(sb.Repo, ".codex", "probe-start")) {
			t.Fatalf("SessionStart marker missing; hooks were not loaded\n%s", run.Stderr)
		}
		if !fileExists(filepath.Join(sb.Repo, ".codex", "probe-end")) {
			t.Errorf("Codex exited successfully without the synchronous SessionEnd marker\n%s", run.Stderr)
		}
	})
}
