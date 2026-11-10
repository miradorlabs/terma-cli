package muse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	museSession = "019a6c2e-8f31-7c40-9b1e-2f6d4e8a1c55"
	museChild   = "019a6c2e-9a02-7d11-8e3f-5b7c9d0e2f66"
	museTurn    = "019a6c2f-0b13-7e22-9f40-6c8d0e1f3a77"
)

// musePayload is a hook's stdin as Muse writes it, with the event's own fields merged in.
func musePayload(t *testing.T, root, event string, extra map[string]any) string {
	t.Helper()
	in := map[string]any{
		"hook_event_name": event, "session_id": museSession, "cwd": root, "transcript_path": nil,
		"model": "muse-spark-1", "permission_mode": "on-request", "turn_id": museTurn,
	}
	for k, v := range extra {
		in[k] = v
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type museFixture struct {
	t     *testing.T
	root  string
	sp    *spool.Spool
	state string
	agent []string
	// detached are the readers the hooks started, each its event and arguments.
	detached [][]string
}

func newMuseFixture(t *testing.T) *museFixture {
	t.Helper()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &museFixture{t: t, root: hookruntest.InitRepo(t), sp: sp, state: t.TempDir(), agent: []string{name}}
}

func (f *museFixture) run(h func(context.Context, hookrun.Env) error, event string, extra map[string]any) {
	f.t.Helper()
	var stdout strings.Builder
	env := hookrun.Env{StateDir: f.state, Now: time.Now(), Cwd: f.root, Policy: hookruntest.Admitting(f.root),
		Stdin: strings.NewReader(musePayload(f.t, f.root, event, extra)), Stdout: &stdout, Spool: f.sp, Version: "test", Agents: f.agent,
		Detach: func(event string, args ...string) { f.detached = append(f.detached, append([]string{event}, args...)) }}
	if err := h(context.Background(), env); err != nil {
		f.t.Fatal(err)
	}
	if stdout.Len() != 0 {
		f.t.Fatalf("%s printed %q: what a hook prints reaches the model", event, stdout.String())
	}
}

func (f *museFixture) spooled() []spool.Event { return hookruntest.Spooled(f.t, f.sp) }

// One turn of a session: its start, the prompt, an edit, the model call with its usage, a
// reminder subagent's call, and the reply, each spooled in the root session and its turn.
func TestMuseTurnIsSpooled(t *testing.T) {
	t.Setenv(museRootEnv, "")
	f := newMuseFixture(t)
	f.run(sessionStart, "SessionStart", map[string]any{"source": "startup"})
	f.run(prompt, "UserPromptSubmit", map[string]any{"prompt": "add a greeting"})
	hookruntest.WriteFile(t, f.root, "hello.txt", "hello\n")
	f.run(postToolUse, "PostToolUse", map[string]any{"tool_name": "write_file", "tool_use_id": "call_1",
		"tool_input": map[string]any{"path": filepath.Join(f.root, "hello.txt"), "content": "hello\n"}, "tool_response": map[string]any{"ok": true}})
	f.run(postToolUse, "PostToolUseFailure", map[string]any{"tool_name": "shell", "tool_use_id": "call_2", "tool_input": map[string]any{"command": "false"}})
	f.run(postLLMCall, "PostLLMCall", map[string]any{"provider": "meta", "request_id": "req_1", "response_id": "resp_1", "finish_reason": "stop",
		"status": "ok", "step": 2, "messages": []any{"never read"}, "usage": map[string]any{"input_tokens": 1200, "output_tokens": 80, "cached_tokens": 950, "cache_read_tokens": 900, "cache_write_tokens": 50, "reasoning_tokens": 30}})
	t.Setenv(museRootEnv, museSession)
	f.run(subagentStart, "SubagentStart", map[string]any{"session_id": museChild, "subagent_id": "skill-reminder", "child_session_id": museChild})
	f.run(postLLMCall, "PostLLMCall", map[string]any{"session_id": museChild, "request_id": "req_2", "usage": map[string]any{"input_tokens": 50, "output_tokens": 5}})
	t.Setenv(museRootEnv, "")
	f.run(stop, "Stop", map[string]any{"last_assistant_message": "Added hello.txt.", "stop_hook_active": false})

	byName := map[string][]spool.Event{}
	for _, e := range f.spooled() {
		if e.SessionID != museSession {
			t.Errorf("%s spooled in session %q, want the root's", e.Name, e.SessionID)
		}
		byName[e.Name] = append(byName[e.Name], e)
	}
	if n := len(byName[semconv.TermaSessionStartEvent]); n != 1 {
		t.Errorf("%d session starts", n)
	}
	if m := byName[semconv.TermaUserMessageEvent]; len(m) != 1 || m[0].Attrs[semconv.TermaMessageTextKey] != "add a greeting" || m[0].Attrs[semconv.TermaTurnIDKey] != museTurn {
		t.Errorf("prompt = %+v", m)
	}
	if m := byName[semconv.TermaAssistantMessageEvent]; len(m) != 1 || m[0].Attrs[semconv.TermaMessageTextKey] != "Added hello.txt." {
		t.Errorf("reply = %+v", m)
	}
	calls := byName[semconv.TermaToolCallEvent]
	if len(calls) != 2 || calls[0].Attrs[semconv.GenAIToolCallIDKey] != "call_1" || calls[0].Attrs[semconv.TermaOperationStatusKey] != "completed" ||
		calls[1].Attrs[semconv.TermaOperationStatusKey] != "error" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if args, _ := calls[0].Attrs[semconv.GenAIToolCallArgumentsKey].(map[string]any); args["content"] != "hello\n" {
		t.Errorf("arguments = %v", calls[0].Attrs[semconv.GenAIToolCallArgumentsKey])
	}
	if touched := byName[semconv.TermaFilesTouchedEvent]; len(touched) != 1 || hookruntest.Joined(touched[0].Attrs[semconv.TermaFilesPathsKey]) != "hello.txt" {
		t.Errorf("files touched = %+v", touched)
	}
	model := byName[semconv.TermaModelCallEvent]
	if len(model) != 2 {
		t.Fatalf("model calls = %+v", model)
	}
	for k, want := range map[string]any{
		semconv.TermaModelCallIDKey: "req_1", semconv.GenAIResponseIDKey: "resp_1", semconv.GenAIRequestModelKey: "muse-spark-1",
		semconv.GenAIUsageInputTokensKey: float64(1200), semconv.GenAIUsageOutputTokensKey: float64(80),
		semconv.GenAIUsageCacheReadInputTokensKey: float64(900), semconv.GenAIUsageCacheWriteInputTokensKey: float64(50),
		semconv.GenAIUsageReasoningOutputTokensKey: float64(30),
		semconv.TermaStepIndexKey:                  float64(2), semconv.TermaTurnIDKey: museTurn,
	} {
		if model[0].Attrs[k] != want {
			t.Errorf("model call %s = %v, want %v", k, model[0].Attrs[k], want)
		}
	}
	if model[1].Attrs[semconv.TermaAgentIDKey] != museChild || model[0].Attrs[semconv.TermaAgentIDKey] != nil {
		t.Errorf("a reminder's call names its thread, the main thread's none: %v / %v", model[1].Attrs[semconv.TermaAgentIDKey], model[0].Attrs[semconv.TermaAgentIDKey])
	}
	if s := byName[semconv.TermaSubagentStartEvent]; len(s) != 1 || s[0].Attrs[semconv.TermaAgentIDKey] != museChild || s[0].Attrs[semconv.GenAIAgentNameKey] != "skill-reminder" {
		t.Errorf("subagent start = %+v", s)
	}
}

// What the person said and what Muse answered are spooled only for a developer who chose
// Muse at setup; the turn's work still is.
func TestMuseConversationNeedsConsent(t *testing.T) {
	t.Setenv(museRootEnv, "")
	f := newMuseFixture(t)
	f.agent = nil
	f.run(prompt, "UserPromptSubmit", map[string]any{"prompt": "secret plan"})
	f.run(stop, "Stop", map[string]any{"last_assistant_message": "secret answer"})
	f.run(postLLMCall, "PostLLMCall", map[string]any{"request_id": "req_1", "usage": map[string]any{"input_tokens": 1}})
	for _, e := range f.spooled() {
		if e.Name == semconv.TermaUserMessageEvent || e.Name == semconv.TermaAssistantMessageEvent {
			t.Fatalf("spooled %s without consent", e.Name)
		}
	}
}

// A long prompt is cut on a rune boundary and says so.
func TestMusePromptIsBounded(t *testing.T) {
	t.Setenv(museRootEnv, "")
	f := newMuseFixture(t)
	f.run(prompt, "UserPromptSubmit", map[string]any{"prompt": strings.Repeat("é", museMaxText)})
	for _, e := range f.spooled() {
		if e.Name != semconv.TermaUserMessageEvent {
			continue
		}
		text, _ := e.Attrs[semconv.TermaMessageTextKey].(string)
		if e.Attrs[semconv.TermaMessageTruncatedKey] != true || len(text) > museMaxText || !strings.HasSuffix(text, "é") {
			t.Fatalf("truncated %v, %d bytes", e.Attrs[semconv.TermaMessageTruncatedKey], len(text))
		}
		return
	}
	t.Fatal("no prompt spooled")
}

// A patch names the files it changes in its headers.
func TestMuseEditedPaths(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n*** Add File: b/c.go\n+package c\n*** Delete File: d.txt\n*** End Patch\n"
	raw, _ := json.Marshal(map[string]string{"input": patch})
	if got := strings.Join(editedPaths("apply_patch", raw), ","); got != "a.go,b/c.go,d.txt" {
		t.Errorf("apply_patch = %q", got)
	}
	if got := editedPaths("read_file", json.RawMessage(`{"path":"a.go"}`)); got != nil {
		t.Errorf("a read named %v", got)
	}
}

// The relay's claim names the root session even for a subagent's hook.
func TestMusePayloadSessionIsTheRoots(t *testing.T) {
	t.Setenv(museRootEnv, museSession)
	s, ok := Agent{}.PayloadSession([]byte(`{"session_id":"` + museChild + `","cwd":"/src/app"}`))
	if !ok || s.ID != museSession || s.AgentID != museChild || s.Cwd != "/src/app" {
		t.Fatalf("PayloadSession = %+v, %v", s, ok)
	}
}

// terma's hooks merge into Muse's settings, its schema version set in a file terma creates,
// and leave the developer's settings and hooks as they were.
func TestMuseHooksMergeIntoSettings(t *testing.T) {
	dir := t.TempDir()
	cmd := func(event string) string { return "terma hook --user " + event }
	plan, err := planHooks(dir, cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]json.RawMessage
	for _, c := range plan.Changes {
		if err := json.Unmarshal(c.After, &created); err != nil {
			t.Fatal(err)
		}
	}
	if string(created["schema_version"]) != "1" {
		t.Errorf("a new settings.json = %s", created["schema_version"])
	}
	var hooks map[string][]struct {
		Hooks []struct {
			Type, Command string
			Timeout       int
			Async         bool
		}
	}
	if err := json.Unmarshal(created["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks) != len(museHooks) {
		t.Errorf("%d events, want %d", len(hooks), len(museHooks))
	}
	llm := hooks["PostLLMCall"]
	if len(llm) != 1 || len(llm[0].Hooks) != 1 || !llm[0].Hooks[0].Async || llm[0].Hooks[0].Command != "terma hook --user muse-post-llm-call" || llm[0].Hooks[0].Type != "command" {
		t.Errorf("PostLLMCall = %+v", llm)
	}
	if stop := hooks["Stop"]; len(stop) != 1 || stop[0].Hooks[0].Async || stop[0].Hooks[0].Timeout != 10 {
		t.Errorf("Stop = %+v", stop)
	}
}

// Delivery asks the agent again before a prompt or reply leaves: Muse consents for a
// developer who chose it, or under global mode, and for no one else.
func TestMuseContentConsent(t *testing.T) {
	for _, tc := range []struct {
		c    hookrun.Consent
		want bool
	}{
		{hookrun.Consent{Agents: []string{name}}, true},
		{hookrun.Consent{Global: true}, true},
		{hookrun.Consent{Agents: []string{"claude"}}, false},
	} {
		if got := (Agent{}).ContentConsented(tc.c); got != tc.want {
			t.Errorf("ContentConsented(%+v) = %v", tc.c, got)
		}
	}
}

// A turn's end starts the detached reader for its session and turn, but not again within
// museReadEvery; the session's end always does. A subagent's stop starts none, and without
// consent no reader runs.
func TestMuseStopStartsTheReader(t *testing.T) {
	t.Setenv(museRootEnv, "")
	f := newMuseFixture(t)
	f.run(stop, "Stop", map[string]any{"last_assistant_message": "done"})
	// The read it started rewrote the session's cursor.
	hookruntest.WriteFile(t, filepath.Join(f.state, museFeedDir), hookrun.EvidenceID(museSession)+".json", "{}")
	f.run(stop, "Stop", map[string]any{"last_assistant_message": "done again"})
	f.run(sessionEnd, "SessionEnd", map[string]any{"reason": "exit"})
	t.Setenv(museRootEnv, museSession)
	f.run(stop, "Stop", map[string]any{"session_id": museChild})
	want := museFeedEvent + " " + museSession + " " + f.root + " " + museTurn + "|" + museFeedEvent + " " + museSession + " " + f.root + " "
	var got []string
	for _, d := range f.detached {
		got = append(got, strings.Join(d, " "))
	}
	if strings.Join(got, "|") != want {
		t.Fatalf("detached = %q, want %q", got, want)
	}

	// Once museReadEvery has passed, a turn's end reads again.
	t.Setenv(museRootEnv, "")
	marker := filepath.Join(f.state, museFeedDir, hookrun.EvidenceID(museSession)+".json")
	past := time.Now().Add(-museReadEvery - time.Second)
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}
	f.detached = nil
	f.run(stop, "Stop", nil)
	if len(f.detached) != 1 {
		t.Fatalf("after %v, detached = %v", museReadEvery, f.detached)
	}

	f.agent, f.detached = nil, nil
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}
	f.run(stop, "Stop", nil)
	if len(f.detached) != 0 {
		t.Fatalf("a reader ran without consent: %v", f.detached)
	}
}
