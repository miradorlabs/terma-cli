package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercises the installed Claude binary without provider credentials or inference:
// both the Messages API and all OTLP destinations are local test servers.
// Run with TERMA_CLAUDE_NATIVE_TEST=1; optionally select TERMA_CLAUDE_BINARY.
func TestClaudeRouteNativeExport(t *testing.T) {
	if os.Getenv("TERMA_CLAUDE_NATIVE_TEST") != "1" {
		t.Skip("set TERMA_CLAUDE_NATIVE_TEST=1 for the native Claude contract test")
	}
	binary := os.Getenv("TERMA_CLAUDE_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("claude")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	exp := Exporter{ProjectID: "route-test", APIKey: "ter_srv_0123456789abcdef", Signals: AllSignals, IncludePrompts: true, IncludeToolContent: true}
	var mu sync.Mutex
	seen := map[string][]string{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_mock","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"TERMA_NATIVE_OK"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":2}}`)
			return
		}
		mu.Lock()
		seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer receiver.Close()
	exp.Endpoint = receiver.URL + "/local"
	global, _ := (Claude{}).ConfigPath()
	globalDoc := map[string]any{"env": map[string]string{
		claudeBetaTracingDetailed: "1", claudeBetaTracingEndpoint: receiver.URL + "/wrongbeta",
		otelEndpoint: receiver.URL + "/wrong", otelHeaders: "Authorization=Bearer WRONG",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": receiver.URL + "/wronglogs",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":  "Authorization=Bearer WRONG",
	}, claudeOtelHeadersHelper: `echo '{"Authorization":"Bearer WRONG"}'`}
	b, _ := json.Marshal(globalDoc)
	if err := os.WriteFile(global, b, 0600); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(t.TempDir(), "route.json")
	helper := filepath.Join(t.TempDir(), "otel-headers")
	if err := (Claude{}).WriteRouteSettings(settingsPath, helper, exp); err != nil {
		t.Fatal(err)
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	profile, _ := json.Marshal(map[string]any{"hasCompletedOnboarding": true, "projects": map[string]any{root: map[string]bool{"hasTrustDialogAccepted": true}}})
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), profile, 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(), "CLAUDE_CONFIG_DIR=" + configDir,
		"ANTHROPIC_API_KEY=dummy", "ANTHROPIC_BASE_URL=" + receiver.URL + "/api", "DISABLE_AUTOUPDATER=1", "DISABLE_ERROR_REPORTING=1",
		"ENABLE_BETA_TRACING_DETAILED=1", "BETA_TRACING_ENDPOINT=" + receiver.URL + "/wrongshellbeta",
		"OTEL_METRIC_EXPORT_INTERVAL=100", "OTEL_LOGS_EXPORT_INTERVAL=100", "OTEL_BSP_SCHEDULE_DELAY=100"}
	run := func(dir string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--settings", settingsPath, "-p", "Reply TERMA_NATIVE_OK", "--model", "claude-sonnet-4-6", "--tools", "", "--max-turns", "1", "--dangerously-skip-permissions")
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "TERMA_NATIVE_OK") {
			t.Fatalf("native Claude failed: %v %s", err, out)
		}
	}
	assertExport := func() {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for _, sig := range []string{"logs", "metrics", "traces"} {
			headers := seen["/local/v1/"+sig]
			if len(headers) == 0 {
				t.Errorf("no %s export; paths: %v", sig, seen)
			}
			for _, h := range headers {
				if h != "Bearer "+exp.APIKey {
					t.Errorf("wrong credential for %s", sig)
				}
			}
		}
		for path := range seen {
			if strings.HasPrefix(path, "/wrong") {
				t.Errorf("global destination won: %s", path)
			}
		}
		seen = map[string][]string{}
	}
	run(root)
	assertExport()
	sub := filepath.Join(root, "subdir")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	run(sub)
	assertExport()
	linked := addRouteWorktree(t, root)
	run(linked)
	assertExport()
	// The helper does not require Terma. Losing it must not prevent agent use either.
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	run(root)
}

func addRouteWorktree(t *testing.T, root string) string {
	t.Helper()
	for _, args := range [][]string{{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"}, {"-C", root, "worktree", "add", "--detach", filepath.Join(t.TempDir(), "linked")}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("worktree setup: %v %s", err, out)
		}
		if args[2] == "worktree" {
			return args[len(args)-1]
		}
	}
	return ""
}
