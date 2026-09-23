package hookrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The payloads below are what agy 1.2.4 wrote to the probe hooks on 2026-09-16, with
// the workspace substituted; agy 1.2.7 wrote the same keys on 2026-09-18. Hooks run from <workspace>/.agents, so the repository has
// to come from workspacePaths; every event expects `{}` back on stdout.
func antigravityPayload(root, event string, extra string) string {
	common := `"artifactDirectoryPath":"/home/dev/.gemini/antigravity-cli/brain/6ac5e722-b53c-4798-8cc5-9d1d035b68da","conversationId":"6ac5e722-b53c-4798-8cc5-9d1d035b68da","modelName":"gemini-3.8-flash-high","transcriptPath":"/home/dev/.gemini/antigravity-cli/brain/6ac5e722-b53c-4798-8cc5-9d1d035b68da/.system_generated/logs/transcript_full.jsonl","workspacePaths":["` + root + `"]`
	_ = event
	return `{` + common + `,` + extra + `}`
}

func TestAntigravityConversationIsStampedOnItsCommit(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	hookDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: hookDir, Stdin: strings.NewReader(stdin), Stdout: &stdout, Spool: sp, Version: "test"}
	}
	run := func(h func(context.Context, Env) error, stdin string) {
		t.Helper()
		stdout.Reset()
		if err := h(ctx, env(stdin)); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(stdout.String()) != "{}" {
			t.Fatalf("agy expects {} on stdout, got %q", stdout.String())
		}
	}

	// Turn one: a fresh conversation announces itself on its first invocation only.
	run(AntigravityPreInvocation, antigravityPayload(root, "PreInvocation", `"initialNumSteps":1,"invocationNum":0`))
	run(AntigravityPreInvocation, antigravityPayload(root, "PreInvocation", `"initialNumSteps":3,"invocationNum":1`))
	starts := 0
	for _, e := range spooled(t, sp) {
		if e.Name == EventSessionStart {
			starts++
			if e.Attrs["tool"] != antigravityTool || e.Attrs["model"] != "gemini-3.8-flash-high" {
				t.Fatalf("start attrs: %v", e.Attrs)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("expected one session start, got %d", starts)
	}

	writeFile(t, root, "hello.txt", "hello\n")
	run(AntigravityPostToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":2,"toolCall":{"args":{"CodeContent":"hello","Description":"Create hello.txt","Overwrite":true,"TargetFile":"`+filepath.Join(root, "hello.txt")+`","toolAction":"Creating file","toolSummary":"Create hello.txt"},"name":"write_to_file"}`))
	run(AntigravityPostToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":4,"toolCall":{"args":{"AllowMultiple":false,"EndLine":1,"Instruction":"Change hello to goodbye","ReplacementContent":"goodbye","StartLine":1,"TargetContent":"hello","TargetFile":"`+filepath.Join(root, "hello.txt")+`","toolAction":"Editing file","toolSummary":"Update hello.txt"},"name":"replace_file_content"}`))
	// A read is not an edit, and a file outside the repository is not ours.
	run(AntigravityPostToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":6,"toolCall":{"args":{"AbsolutePath":"`+filepath.Join(root, "hello.txt")+`"},"name":"view_file"}`))
	run(AntigravityPostToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":7,"toolCall":{"args":{"TargetFile":"/etc/hosts"},"name":"write_to_file"}`))
	// spooled drains, so the steps' events are read once and examined three ways.
	stepEvents := spooled(t, sp)
	touched := 0
	for _, e := range stepEvents {
		if e.Name == EventFilesTouched {
			touched++
			if e.Attrs["files"] != "hello.txt" || e.Attrs["tool"] != antigravityTool {
				t.Fatalf("files touched attrs: %v", e.Attrs)
			}
		}
	}
	if touched != 2 {
		t.Fatalf("expected two files-touched events (write, replace), got %d", touched)
	}
	// Every step is a tool call — the read and the write outside the repository too —
	// keyed on agy's own step index and placed in the turn it belongs to.
	var calls []map[string]any
	for _, e := range stepEvents {
		if e.Name == EventToolCall {
			calls = append(calls, e.Attrs)
		}
	}
	wantCalls := [][2]string{{"write_to_file", "step-2"}, {"replace_file_content", "step-4"}, {"view_file", "step-6"}, {"write_to_file", "step-7"}}
	if len(calls) != len(wantCalls) {
		t.Fatalf("expected %d tool calls, got %d: %v", len(wantCalls), len(calls), calls)
	}
	for i, want := range wantCalls {
		c := calls[i]
		if c["tool_name"] != want[0] || c["tool_call_id"] != want[1] || c["turn_id"] != "turn-1" ||
			c["status"] != "completed" || c["tool"] != antigravityTool || c["hook_event"] != "PostToolUse" {
			t.Errorf("call %d = %v, want %v in turn-1, completed", i, c, want)
		}
		if _, ok := c["duration_ms"]; ok {
			t.Errorf("call %d reports a duration agy never gave: %v", i, c)
		}
	}
	// What a call was given is content, and none of it travels: not the file body, not
	// the replacement, not a path.
	for _, e := range stepEvents {
		if e.Name != EventToolCall {
			continue
		}
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok && (strings.Contains(s, "goodbye") || strings.Contains(s, "hosts") || strings.Contains(s, "hello")) {
				t.Errorf("tool call leaks an argument in %q: %v", k, v)
			}
		}
	}
	// An edit's files-touched event is the same step, not a second call: it says so by
	// carrying the call's ids.
	for _, e := range stepEvents {
		if e.Name == EventFilesTouched && (e.Attrs["turn_id"] != "turn-1" || !strings.HasPrefix(e.Attrs["tool_call_id"].(string), "step-")) {
			t.Errorf("files touched is not tied to its call: %v", e.Attrs)
		}
	}

	run(AntigravityPostInvocation, antigravityPayload(root, "PostInvocation", `"initialNumSteps":7,"invocationNum":3`))
	run(AntigravityStop, antigravityPayload(root, "Stop", `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`))
	var stop map[string]any
	observations := 0
	for _, e := range spooled(t, sp) {
		if e.Name == EventSessionObservation {
			observations++
			if e.Attrs["hook_event"] == "Stop" {
				stop = e.Attrs
			}
		}
	}
	// (The turn's start was observed too — sequence 1 — and drained with the session start.)
	if observations != 2 || stop == nil {
		t.Fatalf("expected PostInvocation and Stop observations, got %d", observations)
	}
	for k, want := range map[string]any{
		"tool": antigravityTool, "termination_reason": "NO_TOOL_CALL", "status": "ok", "fully_idle": true,
		"execution_num": float64(0), "usage_status": "unavailable", "funding_status": "unavailable", "ordering": "local_receipt",
		"observation_sequence": float64(3), "model": "gemini-3.8-flash-high", "turn_id": "turn-1",
	} {
		if stop[k] != want {
			t.Errorf("stop[%q] = %v (%T), want %v", k, stop[k], stop[k], want)
		}
	}
	if stop["observation_id"] == "" || stop["source_stream"] == "" {
		t.Fatalf("observation lacks durable identity: %v", stop)
	}
	for _, forbidden := range []string{"error", "transcript_path", "artifact_directory_path"} {
		if _, ok := stop[forbidden]; ok {
			t.Errorf("observation carries %s, which is content or a private path", forbidden)
		}
	}

	if _, err := gitx.Git(ctx, root, "add", "hello.txt"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	if err := PrepareCommitMsg(ctx, Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: 6ac5e722-b53c-4798-8cc5-9d1d035b68da") || !strings.Contains(string(data), "Agent-Tool: antigravity") {
		t.Fatalf("commit not stamped for Antigravity:\n%s", data)
	}
}

// A turn in a resumed or continuing conversation refreshes the active session without
// announcing a second start, and the second turn's Stop keeps the session fresh for the
// commit that follows.
func TestAntigravityLaterTurnsRefreshWithoutRestarting(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if err := AntigravityPreInvocation(ctx, env(antigravityPayload(root, "PreInvocation", `"initialNumSteps":9,"invocationNum":0`))); err != nil {
		t.Fatal(err)
	}
	if err := AntigravityStop(ctx, env(antigravityPayload(root, "Stop", `"error":"boom","executionNum":1,"fullyIdle":false,"terminationReason":"ERROR"`))); err != nil {
		t.Fatal(err)
	}
	for _, e := range spooled(t, sp) {
		if e.Name == EventSessionStart {
			t.Fatal("a continuing conversation must not announce a new session")
		}
		if e.Name == EventSessionObservation && e.Attrs["hook_event"] == "Stop" {
			if e.Attrs["status"] != "error" || e.Attrs["termination_reason"] != "ERROR" || e.Attrs["turn_id"] != "turn-9" {
				t.Fatalf("stop attrs: %v", e.Attrs)
			}
			if _, ok := e.Attrs["error"]; ok {
				t.Fatal("error text must not travel")
			}
		}
	}
	// The active-session fallback still claims the commit: the turn kept it fresh.
	writeFile(t, root, "b.txt", "x\n")
	if _, err := gitx.Git(ctx, root, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("work\n"), 0o644)
	if err := PrepareCommitMsg(ctx, Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Tool: antigravity") {
		t.Fatalf("active session fallback did not claim the commit:\n%s", data)
	}
}

func TestAntigravityHandlersIgnoreBadInputAndStillAck(t *testing.T) {
	ctx := context.Background()
	t.Setenv(antigravityConversationEnv, "")
	for _, stdin := range []string{"", "not json", `{"modelName":"x"}`, `{"conversationId":"../../etc"}`, `{"conversationId":"c1","toolCall":{"name":"write_to_file","args":"not-an-object"}}`} {
		for name, h := range map[string]func(context.Context, Env) error{
			"pre": AntigravityPreInvocation, "tool": AntigravityPostToolUse, "post": AntigravityPostInvocation, "stop": AntigravityStop,
		} {
			var out bytes.Buffer
			if err := h(ctx, Env{Cwd: t.TempDir(), Stdin: strings.NewReader(stdin), Stdout: &out}); err != nil {
				t.Errorf("%s(%q) = %v", name, stdin, err)
			}
			if strings.TrimSpace(out.String()) != "{}" {
				t.Errorf("%s(%q): stdout %q, want {}", name, stdin, out.String())
			}
		}
	}
}

// agy sets the conversation in the hook's environment as well as the payload; a
// payload that omits it still finds its session.
func TestAntigravityConversationFallsBackToTheEnvironment(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv(antigravityConversationEnv, "env-conv-1")
	stdin := `{"workspacePaths":["` + root + `"],"modelName":"gemini-3.8-pro","initialNumSteps":1,"invocationNum":0}`
	if err := AntigravityPreInvocation(context.Background(), Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	events := spooled(t, sp)
	if len(events) != 2 || events[0].Name != EventSessionStart || events[1].Name != EventSessionObservation {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events {
		if e.SessionID != "env-conv-1" {
			t.Fatalf("event outside the conversation: %+v", e)
		}
	}
}

// agy names no turn, and nothing in its payloads does either: `invocationNum` restarts
// every turn, `initialNumSteps` moves with every invocation, and Stop's `executionNum`
// was 0 on both turns of one resumed conversation (agy 1.2.7, 2026-09-18 — the numbers
// below are that recording). The turn is where it began.
func TestAntigravityTurnsAreNamedByWhereTheyBegan(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	run := func(h func(context.Context, Env) error, extra string) {
		t.Helper()
		if err := h(ctx, Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(antigravityPayload(root, "", extra)), Spool: sp, Version: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	// A step before terma has seen any turn begin: reported, and placed in no turn.
	run(AntigravityPostToolUse, `"error":"","stepIdx":0,"toolCall":{"args":{},"name":"view_file"}`)

	run(AntigravityPreInvocation, `"initialNumSteps":1,"invocationNum":0`)
	run(AntigravityPostToolUse, `"error":"","stepIdx":2,"toolCall":{"args":{"CommandLine":"ls missing"},"name":"run_command"}`)
	run(AntigravityPostInvocation, `"initialNumSteps":1,"invocationNum":0`)
	run(AntigravityPreInvocation, `"initialNumSteps":3,"invocationNum":1`)
	run(AntigravityPostInvocation, `"initialNumSteps":3,"invocationNum":1`)
	run(AntigravityStop, `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)

	run(AntigravityPreInvocation, `"initialNumSteps":9,"invocationNum":0`)
	run(AntigravityPostToolUse, `"error":"tool crashed","stepIdx":11,"toolCall":{"args":{},"name":"view_file"}`)
	run(AntigravityPostInvocation, `"initialNumSteps":9,"invocationNum":0`)
	run(AntigravityStop, `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)

	var got []string
	for _, e := range spooled(t, sp) {
		turn, _ := e.Attrs["turn_id"].(string)
		switch e.Name {
		case EventToolCall:
			got = append(got, "call "+e.Attrs["tool_call_id"].(string)+" "+e.Attrs["status"].(string)+" "+turn)
			if _, ok := e.Attrs["error"]; ok {
				t.Error("a tool's error text must not travel")
			}
		case EventSessionObservation:
			got = append(got, e.Attrs["hook_event"].(string)+" "+turn)
		}
	}
	want := []string{
		"call step-0 completed ",
		"PreInvocation turn-1", "call step-2 completed turn-1", "PostInvocation turn-1", "PostInvocation turn-1", "Stop turn-1",
		"PreInvocation turn-9", "call step-11 error turn-9", "PostInvocation turn-9", "Stop turn-9",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A payload naming neither a tool nor a step is not a call terma can describe.
func TestAntigravityToolCallNeedsANameOrAStep(t *testing.T) {
	if _, ok := antigravityToolCallAttrs(&antigravityHookInput{}, "turn-1"); ok {
		t.Fatal("a payload with no tool and no step made a tool call")
	}
	a, ok := antigravityToolCallAttrs(&antigravityHookInput{StepIdx: []byte("5")}, "")
	if !ok || a["tool_call_id"] != "step-5" || a["step_idx"] != int64(5) {
		t.Fatalf("a step alone identifies a call: %v %v", a, ok)
	}
	if _, has := a["turn_id"]; has {
		t.Fatalf("an unknown turn stays missing: %v", a)
	}
}

func TestAntigravityEditedPaths(t *testing.T) {
	cases := map[string][]string{
		`{"name":"write_to_file","args":{"TargetFile":"/w/a.go","CodeContent":"x"}}`:       {"/w/a.go"},
		`{"name":"multi_replace_file_content","args":{"TargetFile":"/w/b.go"}}`:            {"/w/b.go"},
		`{"name":"str_replace_editor","args":{"command":"str_replace","path":"/w/c.py"}}`:  {"/w/c.py"},
		`{"name":"str_replace_editor","args":{"command":"view","path":"/w/c.py"}}`:         nil,
		`{"name":"view_file","args":{"AbsolutePath":"/w/a.go"}}`:                           nil,
		`{"name":"run_command","args":{"CommandLine":"sed -i s/a/b/ /w/a.go","Cwd":"/w"}}`: nil,
		`{"name":"notebook_edit","args":{"notebook_path":"/w/n.ipynb"}}`:                   {"/w/n.ipynb"},
		`{"name":"delete_file","args":{"TargetFile":"/w/old.go"}}`:                         {"/w/old.go"},
		`{"name":"write_to_file","args":{"TargetFile":""}}`:                                nil,
		`{"name":"write_to_file"}`: nil,
	}
	for raw, want := range cases {
		in := &antigravityHookInput{}
		if err := jsonUnmarshalInto(raw, in); err != nil {
			t.Fatal(err)
		}
		got := antigravityEditedPaths(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v want %v", raw, got, want)
		}
	}
}
