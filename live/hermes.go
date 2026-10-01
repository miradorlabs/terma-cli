package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Hermes (Nous Research) has no usable OTLP export: terma's plugin
// (internal/agents/hermes/plugin), written into $HERMES_HOME/plugins/terma and enabled in its
// config.yaml, is its exporter and calls `terma hook hermes-*`. `terma relay setup
// --harness hermes` writes it pointed at the relay; a direct run writes the same plugin
// pointed at the receiver. Only the installed build is tested (`hermes` on PATH runs
// its own virtualenv by absolute path, so a sandboxed HOME is fine).

func forEachHermes(t *testing.T, run func(t *testing.T, b Binary)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_LIVE=1")
	}
	path, err := exec_LookPath("hermes")
	if err != nil {
		Record(t.Name(), "not run", "hermes is not installed")
		t.Skip("hermes is not installed")
	}
	b := Binary{Harness: "hermes", Version: numberOf(Version(path)), Path: path, Installed: true}
	t.Run(b.Label(), func(t *testing.T) { run(t, b) })
}

// hermesHome is the sandbox's HERMES_HOME.
func (sb *Sandbox) hermesHome() string { return filepath.Join(sb.Home, ".hermes") }

// UseHermes points the sandbox's Hermes at an OpenAI-compatible endpoint, as its only
// model, and puts HERMES_HOME in every environment the sandbox builds, so terma's
// setup, Hermes and the hooks it starts agree on it.
func (sb *Sandbox) UseHermes(url string) {
	sb.ExtraEnv = append(sb.ExtraEnv, "HERMES_HOME="+sb.hermesHome())
	sb.writeAbs(filepath.Join(sb.hermesHome(), "config.yaml"), fmt.Sprintf(`model:
  default: m
  provider: custom
  base_url: %s/v1
  api_key: synthetic
  context_length: 128000
`, url))
}

// UseHermesPluginDirect writes terma's plugin pointed straight at the receiver and
// enables it, as the direct half of a comparison.
func (sb *Sandbox) UseHermesPluginDirect() {
	t := sb.T
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("..", "internal", "agents", "hermes", "plugin", "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "internal", "agents", "hermes", "plugin", "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "CONFIG_JSON = None  # terma:config"
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "endpoint": sb.Receiver.URL(), "headers": map[string]string{"Authorization": "Bearer " + liveKey},
		"includePrompts": true, "includeToolContent": true, "hookCommand": []string{sb.Terma, "hook"},
	})
	if !bytes.Contains(tmpl, []byte(marker)) {
		t.Fatal("the Hermes plugin template has no configuration line")
	}
	quoted, _ := json.Marshal(string(cfg)) // a JSON string is a Python string literal here
	dir := filepath.Join(sb.hermesHome(), "plugins", "terma")
	sb.writeAbs(filepath.Join(dir, "__init__.py"), strings.Replace(string(tmpl), marker, "CONFIG_JSON = "+string(quoted), 1))
	sb.writeAbs(filepath.Join(dir, "plugin.yaml"), string(manifest))
	sb.hermes("plugins", "enable", "terma")
}

// hermes runs a Hermes subcommand in the sandbox.
func (sb *Sandbox) hermes(args ...string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hermes", args...)
	cmd.Dir = sb.Repo
	cmd.Env = sb.termaEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermes %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// HermesRun runs one `hermes chat -q` in dir, tools approved, and returns its output.
func (sb *Sandbox) HermesRun(b Binary, dir, prompt string) string {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, "chat", "-q", prompt, "-Q", "--yolo")
	cmd.Dir = dir
	cmd.Env = sb.termaEnv()
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hermes chat: %v\n%s", err, out.String())
	}
	return out.String()
}

// hermesProvider is an OpenAI-compatible endpoint the way Hermes uses one: it lists
// its model (GET /v1/models, which Hermes probes), answers the session-title request
// (not streamed), and in the turn asks once for tool with args (none when tool is
// empty) before replying, streaming with usage.
func hermesProvider(calls *atomic.Int32, tool string, args map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m","object":"model","context_length":128000}]}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		n := calls.Add(1)
		answered := false
		for _, m := range req.Messages {
			answered = answered || m.Role == "tool"
		}
		msg := map[string]any{"role": "assistant", "content": "TERMA_TELEMETRY_REPLY"}
		finish := "stop"
		if tool != "" && !answered && req.Stream {
			a, _ := json.Marshal(args)
			msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function",
				"function": map[string]any{"name": tool, "arguments": string(a)}}}}
			finish = "tool_calls"
		}
		usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 7, "total_tokens": 107}
		id := fmt.Sprintf("chatcmpl-%d", n)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "created": 1, "model": "m",
				"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		delta := map[string]any{"role": "assistant"}
		if c, ok := msg["content"].(string); ok {
			delta["content"] = c
		}
		if tc, ok := msg["tool_calls"]; ok {
			delta["tool_calls"] = tc
		}
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta}}})
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}
