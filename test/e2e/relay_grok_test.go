package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// grokProvider is an OpenAI-compatible stub for grok's custom model: it lists the model, and
// answers a streamed chat completion whose last message is the person's, when the request
// offers grok's shell tool, with one call of it running cmd; any other request (grok's own
// title and suggestion calls, or the turn after the tool's result) gets a reply. Every stream
// ends with a usage chunk, which grok exports as the call's tokens.
func grokProvider(calls *atomic.Int32, cmd string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"fake"}]}`, grokModel)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		n := calls.Add(1)
		shell := slices.ContainsFunc(req.Tools, func(t struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}) bool {
			return t.Function.Name == "run_terminal_command"
		})
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Role
		}
		id := fmt.Sprintf("chatcmpl_%d", n)
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(choices []any, extra map[string]any) {
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": req.Model, "choices": choices}
			for k, v := range extra {
				chunk[k] = v
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		finish := "stop"
		if cmd != "" && shell && last == "user" {
			args, _ := json.Marshal(map[string]string{"command": cmd, "description": "workload"})
			emit([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_fake_%d", n), "type": "function",
					"function": map[string]any{"name": "run_terminal_command", "arguments": string(args)}}}}}}, nil)
			finish = "tool_calls"
		} else {
			emit([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "TERMA_TELEMETRY_REPLY"}}}, nil)
		}
		emit([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, nil)
		emit([]any{}, map[string]any{"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 20, "total_tokens": 1020,
			"prompt_tokens_details": map[string]any{"cached_tokens": 400}, "completion_tokens_details": map[string]any{"reasoning_tokens": 5}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// grok through the relay, set up by `terma setup --harness grok` in a scratch GROK_HOME: one
// headless turn with a shell call. The relay forwards grok's model call, tool decision, tool
// result and reply with its stamps; the hooks spool the session's start and its prompt; the
// export starts with the per-signal keys setup writes and no otel_endpoint; and a hook process
// carries GROK_HOOK_EVENT while the shell tool's command does not.
func TestRelayGrok(t *testing.T) {
	forEachGrok(t, func(t *testing.T, b Binary) {
		track(t)
		sb := New(t, Isolated)
		marks := filepath.Join(sb.Dir, "marks")
		if err := os.MkdirAll(marks, 0o700); err != nil {
			t.Fatal(err)
		}
		hookEnv, toolEnv := filepath.Join(marks, "hook-env"), filepath.Join(marks, "tool-env")
		var calls atomic.Int32
		provider := httptest.NewServer(grokProvider(&calls, "env > "+toolEnv+"; printf ran"))
		t.Cleanup(provider.Close)
		sb.UseGrok(provider.URL)
		// A probe of the test's own beside terma's hooks file: what grok gives a hook process.
		sb.writeAbs(filepath.Join(sb.grokHome(), "hooks", "probe.json"),
			`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"env > `+hookEnv+`"}]}]}}`+"\n")
		sb.SetupAgents = append(sb.SetupAgents, "grok")
		sb.RelayAgents = []string{"grok"}
		out := sb.Setup(nil, "--team", sb.ProjectID, "--harness", sb.setupList())
		if !strings.Contains(out, "Do you trust the contents of this directory?") {
			t.Errorf("setup did not say grok asks to trust a folder:\n%s", out)
		}
		hooks, err := os.ReadFile(filepath.Join(sb.grokHome(), "hooks", "terma.json"))
		if err != nil || !strings.Contains(string(hooks), "hook --user grok-user-prompt-submit") {
			t.Fatalf("terma setup wrote no grok hooks: %v\n%s", err, hooks)
		}
		sb.UseRelay(RelayOptions{Start: true, Content: true})
		config, err := os.ReadFile(filepath.Join(sb.grokHome(), "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"otel_logs_endpoint = \"http://" + sb.relayAddr + "/", "[model." + grokModel + "]", "remote_fetch = false"} {
			if !strings.Contains(string(config), want) {
				t.Errorf("config.toml lacks %q:\n%s", want, config)
			}
		}
		for _, never := range []string{"otel_endpoint", "otel_protocol", "otel_metrics_exporter"} {
			if strings.Contains(string(config), never+" ") {
				t.Errorf("setup wrote %s:\n%s", never, config)
			}
		}

		const prompt = "Run the workload. TERMA_GROK_PROMPT"
		sb.GrokRun(b, sb.Repo, prompt)
		if calls.Load() < 2 {
			t.Fatalf("grok made %d model calls, so the turn proves nothing", calls.Load())
		}

		events := []string{"grok_code.api_request", "grok_code.tool_decision", "grok_code.tool_result", "grok_code.assistant_response"}
		byName := map[string]LogRecord{}
		sb.Receiver.WaitLogs(20*time.Second, func(l LogRecord) bool {
			if slices.Contains(events, l.EventName) {
				byName[l.EventName] = l
			}
			return len(byName) == len(events)
		})
		for _, name := range events {
			l, ok := byName[name]
			if !ok {
				t.Errorf("the relay forwarded no %s; relay %v", name, sb.RelayStats())
				continue
			}
			for key, want := range map[string]string{
				"vcs.repository.url.full": "https://github.com/acme/repo", "terma.repository.root": sb.Repo,
				"terma.working_directory": sb.Repo, "terma.home_directory": sb.Home,
			} {
				if got := l.Resource[key]; got != want {
					t.Errorf("%s: %s = %q, want %q", name, key, got, want)
				}
			}
			if l.Resource["terma.version"] == "" || l.Scope != "ai.xai.grok_code" {
				t.Errorf("%s: resource %v, scope %q", name, l.Resource, l.Scope)
			}
		}
		if l := byName["grok_code.tool_result"]; !strings.Contains(l.Attrs["full_command"], toolEnv) {
			t.Errorf("the shell call's command did not reach Terma with tool content collected: %v", l.Attrs)
		}

		starts := sb.WaitEvents("terma.session.start", "", 15*time.Second)
		if len(starts) == 0 || starts[0].Attrs["gen_ai.main_agent.name"] != "grok" {
			t.Fatalf("grok announced no session: %+v", starts)
		}
		sid := starts[0].SessionID
		if l := byName["grok_code.api_request"]; l.Attrs["session.id"] != sid {
			t.Errorf("the export's session %q is not the hooks' %q", l.Attrs["session.id"], sid)
		}
		prompts := sb.WaitEvents("terma.user.message", sid, 15*time.Second)
		if len(prompts) != 1 || prompts[0].Attrs["terma.message.text"] != prompt || prompts[0].Attrs["terma.message.id"] == "" {
			t.Errorf("prompt events %+v", prompts)
		}

		hookData, err := os.ReadFile(hookEnv)
		if err != nil || !strings.Contains(string(hookData), "\nGROK_HOOK_EVENT=") && !strings.HasPrefix(string(hookData), "GROK_HOOK_EVENT=") {
			t.Errorf("a hook process lacks GROK_HOOK_EVENT: %v", err)
		}
		toolData, err := os.ReadFile(toolEnv)
		if err != nil {
			t.Fatalf("grok's shell tool never ran: %v", err)
		}
		if strings.Contains(string(toolData), "GROK_HOOK_EVENT=") {
			t.Error("the shell tool's command carries GROK_HOOK_EVENT: terma's hooks started from it would stand down")
		}
		if strings.Contains(string(toolData), sb.relayAddr) {
			t.Error("the shell tool's command inherited the relay's endpoint")
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		t.Logf("model calls %d, relay %v", calls.Load(), c)
		for name, n := range c {
			if strings.HasPrefix(name, "unclassified.") && name != "unclassified.user.email" && n > 0 {
				t.Errorf("the relay withheld a key no rule names: %s", name)
			}
		}
	})
}
