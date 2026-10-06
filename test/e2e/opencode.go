package e2e

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// OpenCode (sst/opencode) runs through terma's own plugin, which exports OTLP/JSON
// itself: the one harness whose telemetry reaches the relay as JSON, and whose hooks
// the plugin calls rather than the harness.

// OpenCodeBinaries resolves the OpenCode builds to test (TERMA_E2E_OPENCODE_VERSIONS,
// the same spec as Claude's and Codex's).
func OpenCodeBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "opencode", os.Getenv("TERMA_E2E_OPENCODE_VERSIONS"), func() ([]string, error) { return npmReleases("opencode-ai") }, ensureOpenCode)
}

func forEachOpenCode(t *testing.T, run func(t *testing.T, b Binary, newest bool)) {
	t.Helper()
	if !Enabled() {
		t.Skip("live tests run only with TERMA_E2E=1")
	}
	builds := OpenCodeBinaries(t)
	if len(builds) == 0 {
		t.Skip("no OpenCode build to test")
	}
	for i, b := range builds {
		t.Run(b.Label(), func(t *testing.T) { run(t, b, i == len(builds)-1) })
	}
}

// opencodePlatform is the npm package holding this machine's native binary.
func opencodePlatform() string {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	return "opencode-" + runtime.GOOS + "-" + arch
}

func ensureOpenCode(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "opencode", version)
	path := filepath.Join(dir, "opencode")
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	pkg := opencodePlatform()
	url := fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-%s.tgz", pkg, pkg, version)
	data, err := fetch(url)
	if err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("opencode %s: no binary in %s", version, url)
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg || h.Name != "package/bin/opencode" {
			continue
		}
		bin, err := io.ReadAll(io.LimitReader(tr, 512<<20))
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, bin, 0o755); err != nil {
			return "", err
		}
		return abs(path), nil
	}
}

// openAIChatProvider is a deterministic OpenAI-compatible chat completions endpoint
// (what OpenCode's @ai-sdk/openai-compatible provider speaks), streaming one reply.
func openAIChatProvider(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		id := fmt.Sprintf("chatcmpl_%d", n)
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "TERMA_TELEMETRY_REPLY"}}}})
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// UseOpenCodeProvider points the sandbox's OpenCode at an OpenAI-compatible
// endpoint, as its only model.
func (sb *Sandbox) UseOpenCodeProvider(url string) {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"fake": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Fake",
			"options": map[string]any{"baseURL": url + "/v1", "apiKey": "synthetic"},
			"models":  map[string]any{"m": map[string]any{"name": "m"}},
		}},
		"model": "fake/m", "autoupdate": false, "share": "disabled",
		// Headless runs must never stop to ask.
		"permission": map[string]any{"bash": "allow", "edit": "allow", "webfetch": "allow"},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	sb.writeAbs(filepath.Join(sb.Home, ".config", "opencode", "opencode.json"), string(data)+"\n")
}

// directOpenCode installs terma's OpenCode plugin with the receiver as its endpoint,
// unless the scenario uses the relay: the plugin a developer's machine held
// before the relay was the only route.
func (sb *Sandbox) directOpenCode() {
	t := sb.T
	t.Helper()
	if sb.relayed {
		return
	}
	src, err := os.ReadFile("../../internal/agents/opencode/plugin/terma.js")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"version": 1, "endpoint": sb.Receiver.URL(),
		"headers": map[string]string{"Authorization": "Bearer " + liveKey}, "signals": []string{"traces", "logs", "metrics"},
		"includePrompts": true, "includeToolContent": true,
		"resourceAttributes": map[string]string{"mirador.project.id": sb.ProjectID, "service.name": "opencode"},
		"hookCommand":        []string{"terma", "hook"}})
	const placeholder = "const CONFIG = null /* terma:config */"
	if !bytes.Contains(src, []byte(placeholder)) {
		t.Fatal("the OpenCode plugin has no config placeholder")
	}
	sb.writeAbs(filepath.Join(sb.Home, ".config", "opencode", "plugins", "terma.js"), strings.Replace(string(src), placeholder, "const CONFIG = "+string(cfg), 1))
	sb.useLiveKey()
}

// OpenCodeRun runs one headless `opencode run` in dir (continuing session when it is
// set) and returns the session id OpenCode reports.
func (sb *Sandbox) OpenCodeRun(b Binary, dir, session, prompt string) string {
	t := sb.T
	t.Helper()
	args := []string{"run", "--format", "json", "--dir", dir}
	if session != "" {
		args = append(args, "--session", session)
	}
	args = append(args, prompt)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, args...)
	cmd.Dir = dir
	cmd.Env = append(sb.termaEnv(), "XDG_CONFIG_HOME="+filepath.Join(sb.Home, ".config"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode run: %v\n%s\n%s", err, out, stderr.String())
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if id, _ := ev["sessionID"].(string); id != "" {
			return id
		}
	}
	t.Fatalf("opencode run reported no session:\n%s", out)
	return ""
}
