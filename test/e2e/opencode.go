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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// OpenCode (sst/opencode) runs through terma's own plugin, which exports OTLP/JSON
// itself: the one harness whose telemetry reaches the relay as JSON, and whose hooks
// the plugin calls rather than the harness.

// OpenCodeBinaries resolves the OpenCode builds to test (TERMA_E2E_OPENCODE_VERSIONS,
// the same spec as Claude's and Codex's), of either major: OpenCode 1 is published as
// opencode-ai, OpenCode 2 as @opencode/cli.
func OpenCodeBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "opencode", os.Getenv("TERMA_E2E_OPENCODE_VERSIONS"), func() ([]string, error) {
		one, err := npmReleases("opencode-ai")
		if err != nil {
			return nil, err
		}
		two, err := npmReleases("@opencode/cli")
		if err != nil {
			return nil, err
		}
		all := append(one, two...)
		sort.Slice(all, func(i, j int) bool { return versionLess(all[i], all[j]) })
		return all, nil
	}, ensureOpenCode)
}

// openCode2 reports whether b is an OpenCode 2 build, which has its own package, command
// line and tool names.
func openCode2(b Binary) bool { return openCodeMajor(b.Version) >= 2 }

// openCodeMajor is a version's major, 0 when it has none.
func openCodeMajor(version string) int {
	major, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	n, _ := strconv.Atoi(major)
	return n
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

// opencodePlatform is the npm package holding this machine's native binary of version's
// major, and the name its tarball is published under.
func opencodePlatform(version string) (pkg, tarball string) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if openCodeMajor(version) >= 2 {
		tarball = "cli-" + runtime.GOOS + "-" + arch
		return "@opencode/" + tarball, tarball
	}
	tarball = "opencode-" + runtime.GOOS + "-" + arch
	return tarball, tarball
}

func ensureOpenCode(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "opencode", version)
	path := filepath.Join(dir, "opencode")
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	pkg, tarball := opencodePlatform(version)
	url := fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-%s.tgz", pkg, tarball, version)
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
// endpoint, as its only model, and keeps every OpenCode process in the sandbox's folders.
func (sb *Sandbox) UseOpenCodeProvider(url string) {
	sb.isolateOpenCode()
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"fake": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Fake",
			"options": map[string]any{"baseURL": url + "/v1", "apiKey": "synthetic"},
			// gpt-5-m: a name OpenCode offers its patch tool to.
			"models": map[string]any{"m": map[string]any{"name": "m"}, "gpt-5-m": map[string]any{"name": "gpt-5-m"}},
		}},
		"model": "fake/m", "autoupdate": false, "share": "disabled",
		// Headless runs must never stop to ask.
		"permission": map[string]any{"bash": "allow", "edit": "allow", "webfetch": "allow"},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	sb.writeAbs(filepath.Join(sb.Home, ".config", "opencode", "opencode.json"), string(data)+"\n")
}

// isolateOpenCode sets the four XDG folders, for every process the sandbox starts, to their
// defaults under the sandbox's home: where terma looks for OpenCode's files, and the only
// place OpenCode may write. OpenCode 2 shares OpenCode 1's config and data folders and
// database file, and migrates that database, so neither major may fall back to a folder of
// the machine's. terma's own folders are TERMA_CONFIG_DIR's, which these do not move.
func (sb *Sandbox) isolateOpenCode() {
	for _, kv := range sb.ExtraEnv {
		if strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			return
		}
	}
	sb.ExtraEnv = append(sb.ExtraEnv, "XDG_CONFIG_HOME="+filepath.Join(sb.Home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(sb.Home, ".local", "share"), "XDG_STATE_HOME="+filepath.Join(sb.Home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(sb.Home, ".cache"))
}

// requireOpenCodeSandbox refuses to run OpenCode 2 where its folders could be the
// machine's: anywhere but an isolated sandbox whose OpenCode folders isolateOpenCode set.
func (sb *Sandbox) requireOpenCodeSandbox(b Binary) {
	sb.T.Helper()
	if !openCode2(b) {
		return
	}
	isolated := false
	for _, kv := range sb.ExtraEnv {
		isolated = isolated || kv == "XDG_DATA_HOME="+filepath.Join(sb.Home, ".local", "share")
	}
	if sb.Mode != Isolated || !isolated {
		sb.T.Fatalf("OpenCode %s runs only in an isolated sandbox with its own OpenCode folders", b.Version)
	}
}

// UseOpenCode makes b the `opencode` on the sandbox's PATH and sets the machine up again with
// OpenCode chosen, as a developer would: `terma setup` writes the plugin and records OpenCode
// in the profile, which nothing else does. It returns setup's output.
func (sb *Sandbox) UseOpenCode(b Binary) string {
	t := sb.T
	t.Helper()
	sb.isolateOpenCode()
	link := filepath.Join(strings.TrimSuffix(sb.binDir(), ":"), "opencode")
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(b.Path, link); err != nil {
			t.Fatal(err)
		}
	}
	sb.SetupAgents = append(sb.SetupAgents, "opencode")
	sb.RelayAgents = append(sb.RelayAgents, "opencode")
	out := sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
	if !slices.Contains(sb.recordedHarnesses(), "opencode") {
		t.Fatalf("terma setup --harness %s did not record OpenCode: %v\n%s", sb.setupList(), sb.recordedHarnesses(), out)
	}
	sb.Receiver.Reset()
	return out
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
	sb.T.Helper()
	return sb.openCodeRun(b, dir, session, prompt, env, false, nil)
}

// openCodeRunArgs is an `opencode run` of b in dir, before its prompt. OpenCode 1 takes the
// directory as a flag. OpenCode 2 has none, a run works where it starts, and it runs on a
// private server only with --standalone: without it, a run starts or joins the background
// service, which outlives it.
func openCodeRunArgs(b Binary, dir string, service bool) []string {
	if !openCode2(b) {
		return []string{"run", "--format", "json", "--dir", dir}
	}
	if service {
		return []string{"run", "--format", "json"}
	}
	return []string{"run", "--standalone", "--format", "json"}
}

// openCodeRun is OpenCodeRunWith, on the background service when service is set, with flags
// before the prompt.
func (sb *Sandbox) openCodeRun(b Binary, dir, session, prompt string, env []string, service bool, flags []string) string {
	t := sb.T
	t.Helper()
	sb.requireOpenCodeSandbox(b)
	args := openCodeRunArgs(b, dir, service)
	if session != "" {
		args = append(args, "--session", session)
	}
	args = append(append(args, flags...), prompt)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Path, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	// exec keeps the last of two values of a variable.
	cmd.Env = append(sb.termaEnv(), env...)
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

// openCodeIntent is a tool call a scenario means, stated once for every build:
// openCodeCallFor renders it in the build's own tool names and arguments.
type openCodeIntent struct {
	// shell is a command for the shell tool, with description.
	shell, description string
	// writePath and content are a file the write tool writes.
	writePath, content string
	// subagent is the prompt of a general subagent, with description.
	subagent string
	// mcpServer and mcpTool name an MCP tool, called with mcpArgs.
	mcpServer, mcpTool string
	mcpArgs            map[string]any
	// editPath, oldString and newString are a change the edit tool makes.
	editPath, oldString, newString string
	// patchText is a patch for the patch tool (apply_patch on OpenCode 1).
	patchText string
}

// openCodeCallFor renders intent as b's model would ask for it, under id. OpenCode 2 names
// the shell tool `shell` and the subagent tool `subagent`, takes a file as `path`, and offers
// an MCP tool only inside code mode, through `execute`; a model that names an MCP tool flat
// is "repaired" into code mode by 2.4.1, which no scenario relies on.
func openCodeCallFor(b Binary, id string, intent openCodeIntent) openCodeCall {
	two := openCode2(b)
	switch {
	case intent.shell != "":
		name := "bash"
		if two {
			name = "shell"
		}
		return openCodeCall{id: id, name: name, args: map[string]any{"command": intent.shell, "description": intent.description}}
	case intent.writePath != "":
		key := "filePath"
		if two {
			key = "path"
		}
		return openCodeCall{id: id, name: "write", args: map[string]any{key: intent.writePath, "content": intent.content}}
	case intent.editPath != "":
		key := "filePath"
		if two {
			key = "path"
		}
		return openCodeCall{id: id, name: "edit", args: map[string]any{key: intent.editPath, "oldString": intent.oldString, "newString": intent.newString}}
	case intent.patchText != "":
		name := "apply_patch"
		if two {
			name = "patch"
		}
		return openCodeCall{id: id, name: name, args: map[string]any{"patchText": intent.patchText}}
	case intent.subagent != "":
		if two {
			return openCodeCall{id: id, name: "subagent", args: map[string]any{"agent": "general", "description": intent.description, "prompt": intent.subagent}}
		}
		return openCodeCall{id: id, name: "task", args: map[string]any{"description": intent.description, "prompt": intent.subagent, "subagent_type": "general"}}
	case intent.mcpTool != "":
		if two {
			args, _ := json.Marshal(intent.mcpArgs)
			return openCodeCall{id: id, name: "execute", args: map[string]any{"code": fmt.Sprintf("return await tools.%s.%s(%s)", intent.mcpServer, intent.mcpTool, args)}}
		}
		return openCodeCall{id: id, name: intent.mcpServer + "_" + intent.mcpTool, args: intent.mcpArgs}
	}
	panic("an OpenCode intent with no tool")
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
		writeOpenCodeStep(w, step)
	})
}

// writeOpenCodeStep streams step as one chat completion: its tool calls, or its text.
func writeOpenCodeStep(w http.ResponseWriter, step openCodeStep) {
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
}

// openCodeConversationProvider answers like openCodeScriptedProvider, but picks each reply
// by conversation: the steps of the first conversation whose key its request contains, by
// how many tool results that conversation already holds, then TERMA_TELEMETRY_REPLY. A
// subagent that runs beside its parent (OpenCode 2.4.1) gets its own steps whatever order
// the two conversations' requests come in.
func openCodeConversationProvider(byPrompt []openCodeConversation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		step := openCodeStep{text: openCodeTitle}
		if len(body.Tools) > 0 {
			step = openCodeStep{text: "TERMA_TELEMETRY_REPLY"}
			results := 0
			for _, m := range body.Messages {
				if m.Role == "tool" {
					results++
				}
			}
			for _, c := range byPrompt {
				if bytes.Contains(raw, []byte(c.key)) {
					if results < len(c.steps) {
						step = c.steps[results]
					}
					break
				}
			}
		}
		writeOpenCodeStep(w, step)
	})
}

// openCodeConversation is the steps of the conversation whose request contains key.
type openCodeConversation struct {
	key   string
	steps []openCodeStep
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
