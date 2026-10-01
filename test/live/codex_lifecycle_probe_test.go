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
// On Codex 0.158.0 it missed SessionEnd in 3 of 10 runs: `codex exec` gives its in-process
// app-server five seconds to shut down (codex-rs/app-server/src/in_process.rs) while the
// cleanup before run_session_end_hooks allows ten, so the process can exit first. No terma
// timeout can recover it; TERMA_LIVE_KNOWN_UPSTREAM=codex-session-end tolerates it in PR CI.
// Run with TERMA_LIVE_CODEX_LIFECYCLE_PROBE=1 and TERMA_LIVE_CODEX_RUST_LOG for native logs.
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
