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
	"sync"
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
		t.Run(b.Label(), func(t *testing.T) { scenarioOfBuild(t, b); run(t, b, i == len(builds)-1) })
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
// before the relay was the only route. It runs the sandbox's terma by its absolute
// path, as setup writes it.
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
		"hookCommand":        []string{abs(sb.Terma), "hook"}})
	const placeholder = "const CONFIG = null /* terma:config */"
	if !bytes.Contains(src, []byte(placeholder)) {
		t.Fatal("the OpenCode plugin has no config placeholder")
	}
	sb.writeAbs(filepath.Join(sb.Home, ".config", "opencode", "plugins", "terma.js"), strings.Replace(string(src), placeholder, "const CONFIG = "+string(cfg), 1))
}

// OpenCodeRun runs one headless `opencode run` in dir (continuing session when it is
// set) and returns the session id OpenCode reports.
func (sb *Sandbox) OpenCodeRun(b Binary, dir, session, prompt string) string {
	sb.T.Helper()
	return sb.OpenCodeRunWith(b, dir, session, prompt, nil)
}

// OpenCodeRunWith is OpenCodeRun with env laid over the sandbox's environment: a
// variable it names replaces the sandbox's.
func (sb *Sandbox) OpenCodeRunWith(b Binary, dir, session, prompt string, env []string) string {
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
	// exec keeps the last of two values of a variable.
	cmd.Env = append(append(sb.termaEnv(), "XDG_CONFIG_HOME="+filepath.Join(sb.Home, ".config")), env...)
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

// openCodeStep is one scripted reply of the model's: text, or the tool calls it asks for.
type openCodeStep struct {
	text  string
	calls []openCodeCall
}

// openCodeCall is one tool call a scripted reply asks for, under an id the scenario picks.
type openCodeCall struct {
	id, name string
	args     map[string]any
}

// openCodeTitle is what the scripted provider answers OpenCode's title call with.
const openCodeTitle = "Scripted session title"

// openCodeScriptedProvider is an OpenAI-compatible chat completions endpoint that answers
// each conversation request with the next of steps, then TERMA_TELEMETRY_REPLY, and every
// request that offers no tools (OpenCode's title call) with openCodeTitle. calls counts the
// conversation requests.
func openCodeScriptedProvider(calls *atomic.Int32, steps ...openCodeStep) http.Handler {
	var mu sync.Mutex
	next := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		step := openCodeStep{text: openCodeTitle}
		if len(body.Tools) > 0 {
			calls.Add(1)
			mu.Lock()
			step = openCodeStep{text: "TERMA_TELEMETRY_REPLY"}
			if next < len(steps) {
				step = steps[next]
			}
			next++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		chunk := func(delta map[string]any, finish any, usage any) map[string]any {
			c := map[string]any{"id": "chatcmpl_scripted", "object": "chat.completion.chunk", "created": 1, "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			if usage != nil {
				c["usage"] = usage
			}
			return c
		}
		finish := "stop"
		if len(step.calls) > 0 {
			finish = "tool_calls"
			var calls []any
			for i, c := range step.calls {
				arguments, _ := json.Marshal(c.args)
				calls = append(calls, map[string]any{"index": i, "id": c.id, "type": "function",
					"function": map[string]any{"name": c.name, "arguments": string(arguments)}})
			}
			emit(chunk(map[string]any{"role": "assistant", "tool_calls": calls}, nil, nil))
		} else {
			emit(chunk(map[string]any{"role": "assistant", "content": step.text}, nil, nil))
		}
		emit(chunk(map[string]any{}, finish, map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16}))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// echoMCPArg is the first argument that starts the test binary as the echo MCP server.
const echoMCPArg = "terma-echo-mcp"

// RunEchoMCP serves the echo MCP server and exits, when the test binary was started as it.
func RunEchoMCP() {
	if len(os.Args) >= 2 && os.Args[1] == echoMCPArg {
		os.Exit(serveEchoMCP(os.Stdin, os.Stdout))
	}
}

// serveEchoMCP is an MCP server on stdio (newline-delimited JSON-RPC) with one tool, echo,
// whose result is its text argument after "echo: ". It returns when its input closes.
func serveEchoMCP(in io.Reader, out io.Writer) int {
	type message struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}
	enc := json.NewEncoder(out)
	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		_ = enc.Encode(v)
	}
	dec := json.NewDecoder(in)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			return 0
		}
		switch {
		case m.Method == "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			send(map[string]any{"id": m.ID, "result": map[string]any{"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "terma-echo", "version": "1.0.0"}}})
		case m.Method == "tools/list":
			send(map[string]any{"id": m.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "echo",
				"description": "Returns its text.", "inputSchema": map[string]any{"type": "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}}}})
		case m.Method == "tools/call":
			var p struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(m.Params, &p)
			send(map[string]any{"id": m.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments.Text}}}})
		case m.Method == "ping":
			send(map[string]any{"id": m.ID, "result": map[string]any{}})
		case m.Method != "" && len(m.ID) > 0:
			send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}
