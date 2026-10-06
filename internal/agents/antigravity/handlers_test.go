package antigravity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// antigravityPayload is a recorded agy payload with the workspace substituted.
func antigravityPayload(root, event string, extra string) string {
	common := `"artifactDirectoryPath":"/home/dev/.gemini/antigravity-cli/brain/6ac5e722-b53c-4798-8cc5-9d1d035b68da","conversationId":"6ac5e722-b53c-4798-8cc5-9d1d035b68da","modelName":"gemini-3.8-flash-high","transcriptPath":"/home/dev/.gemini/antigravity-cli/brain/6ac5e722-b53c-4798-8cc5-9d1d035b68da/.system_generated/logs/transcript_full.jsonl","workspacePaths":["` + root + `"]`
	_ = event
	return `{` + common + `,` + extra + `}`
}

func TestAntigravityConversationIsStampedOnItsCommit(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	stateDir := t.TempDir()
	hookDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: hookDir, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Stdout: &stdout, Spool: sp, Version: "test"}
	}
	run := func(h func(context.Context, hookrun.Env) error, stdin string) {
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
	run(preInvocation, antigravityPayload(root, "PreInvocation", `"initialNumSteps":1,"invocationNum":0`))
	run(preInvocation, antigravityPayload(root, "PreInvocation", `"initialNumSteps":3,"invocationNum":1`))
	starts := 0
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == semconv.TermaSessionStartEvent {
			starts++
			if e.Attrs[semconv.GenAIMainAgentNameKey] != antigravityTool || e.Attrs[semconv.GenAIRequestModelKey] != "gemini-3.8-flash-high" {
				t.Fatalf("start attrs: %v", e.Attrs)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("expected one session start, got %d", starts)
	}

	hookruntest.WriteFile(t, root, "hello.txt", "hello\n")
	run(postToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":2,"toolCall":{"args":{"CodeContent":"hello","Description":"Create hello.txt","Overwrite":true,"TargetFile":"`+filepath.Join(root, "hello.txt")+`","toolAction":"Creating file","toolSummary":"Create hello.txt"},"name":"write_to_file"}`))
	run(postToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":4,"toolCall":{"args":{"AllowMultiple":false,"EndLine":1,"Instruction":"Change hello to goodbye","ReplacementContent":"goodbye","StartLine":1,"TargetContent":"hello","TargetFile":"`+filepath.Join(root, "hello.txt")+`","toolAction":"Editing file","toolSummary":"Update hello.txt"},"name":"replace_file_content"}`))
	// A read is not an edit, and a file outside the repository is not ours.
	run(postToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":6,"toolCall":{"args":{"AbsolutePath":"`+filepath.Join(root, "hello.txt")+`"},"name":"view_file"}`))
	run(postToolUse, antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":7,"toolCall":{"args":{"TargetFile":"/etc/hosts"},"name":"write_to_file"}`))
	// Spooled drains, so the steps' events are read once.
	stepEvents := hookruntest.Spooled(t, sp)
	touched := 0
	for _, e := range stepEvents {
		if e.Name == semconv.TermaFilesTouchedEvent {
			touched++
			if hookruntest.Joined(e.Attrs[semconv.TermaFilesPathsKey]) != "hello.txt" || e.Attrs[semconv.GenAIMainAgentNameKey] != antigravityTool {
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
		if e.Name == semconv.TermaToolCallEvent {
			calls = append(calls, e.Attrs)
		}
	}
	wantCalls := [][2]string{{"write_to_file", "step-2"}, {"replace_file_content", "step-4"}, {"view_file", "step-6"}, {"write_to_file", "step-7"}}
	if len(calls) != len(wantCalls) {
		t.Fatalf("expected %d tool calls, got %d: %v", len(wantCalls), len(calls), calls)
	}
	for i, want := range wantCalls {
		c := calls[i]
		if c[semconv.GenAIToolNameKey] != want[0] || c[semconv.GenAIToolCallIDKey] != want[1] || c[semconv.TermaTurnIDKey] != "turn-1" ||
			c[semconv.TermaOperationStatusKey] != "completed" || c[semconv.GenAIMainAgentNameKey] != antigravityTool || c[semconv.TermaHookEventKey] != "PostToolUse" {
			t.Errorf("call %d = %v, want %v in turn-1, completed", i, c, want)
		}
		if _, ok := c[semconv.TermaOperationDurationMsKey]; ok {
			t.Errorf("call %d reports a duration agy never gave: %v", i, c)
		}
	}
	// What a call was given is content and never travels.
	for _, e := range stepEvents {
		if e.Name != semconv.TermaToolCallEvent {
			continue
		}
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok && (strings.Contains(s, "goodbye") || strings.Contains(s, "hosts") || strings.Contains(s, "hello")) {
				t.Errorf("tool call leaks an argument in %q: %v", k, v)
			}
		}
	}
	// An edit's files-touched event carries the call's ids: the same step, not a second call.
	for _, e := range stepEvents {
		if e.Name == semconv.TermaFilesTouchedEvent && (e.Attrs[semconv.TermaTurnIDKey] != "turn-1" || !strings.HasPrefix(e.Attrs[semconv.GenAIToolCallIDKey].(string), "step-")) {
			t.Errorf("files touched is not tied to its call: %v", e.Attrs)
		}
	}

	run(postInvocation, antigravityPayload(root, "PostInvocation", `"initialNumSteps":7,"invocationNum":3`))
	run(stop, antigravityPayload(root, "Stop", `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`))
	var stop map[string]any
	observations := 0
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == semconv.TermaSessionObservationEvent {
			observations++
			if e.Attrs[semconv.TermaHookEventKey] == "Stop" {
				stop = e.Attrs
			}
		}
	}
	// (The turn's start was observed too — sequence 1 — and drained with the session start.)
	if observations != 2 || stop == nil {
		t.Fatalf("expected PostInvocation and Stop observations, got %d", observations)
	}
	for k, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaTerminationReasonKey: "NO_TOOL_CALL", semconv.TermaOperationStatusKey: "ok", semconv.TermaAgentFullyIdleKey: true,
		semconv.TermaExecutionNumberKey: float64(0), semconv.TermaUsageStatusKey: "unavailable", semconv.TermaFundingStatusKey: "unavailable",
		semconv.TermaObservationSequenceKey: float64(3), semconv.GenAIRequestModelKey: "gemini-3.8-flash-high", semconv.TermaTurnIDKey: "turn-1",
	} {
		if stop[k] != want {
			t.Errorf("stop[%q] = %v (%T), want %v", k, stop[k], stop[k], want)
		}
	}
	if stop[semconv.TermaObservationIDKey] == "" || stop[semconv.TermaObservationStreamKey] == "" {
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
	if err := hookrun.PrepareCommitMsg(ctx, hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: 6ac5e722-b53c-4798-8cc5-9d1d035b68da") || !strings.Contains(string(data), "Agent-Tool: antigravity") {
		t.Fatalf("commit not stamped for Antigravity:\n%s", data)
	}
}

// A turn in a resumed or continuing conversation refreshes the active session without
// announcing a second start, and what a later turn writes is still stamped on its commit.
func TestAntigravityLaterTurnsRefreshWithoutRestarting(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	stateDir := t.TempDir()
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if err := preInvocation(ctx, env(antigravityPayload(root, "PreInvocation", `"initialNumSteps":9,"invocationNum":0`))); err != nil {
		t.Fatal(err)
	}
	if err := stop(ctx, env(antigravityPayload(root, "Stop", `"error":"boom","executionNum":1,"fullyIdle":false,"terminationReason":"ERROR"`))); err != nil {
		t.Fatal(err)
	}
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == semconv.TermaSessionStartEvent {
			t.Fatal("a continuing conversation must not announce a new session")
		}
		if e.Name == semconv.TermaSessionObservationEvent && e.Attrs[semconv.TermaHookEventKey] == "Stop" {
			if e.Attrs[semconv.TermaOperationStatusKey] != "error" || e.Attrs[semconv.TermaTerminationReasonKey] != "ERROR" || e.Attrs[semconv.TermaTurnIDKey] != "turn-9" {
				t.Fatalf("stop attrs: %v", e.Attrs)
			}
			if _, ok := e.Attrs["error"]; ok {
				t.Fatal("error text must not travel")
			}
		}
	}
	// The continuing conversation still owns what it writes, so a commit of it is stamped.
	hookruntest.WriteFile(t, root, "b.txt", "x\n")
	if err := postToolUse(ctx, env(antigravityPayload(root, "PostToolUse", `"error":"","stepIdx":2,"toolCall":{"args":{"CodeContent":"x","TargetFile":"`+filepath.Join(root, "b.txt")+`"},"name":"write_to_file"}`))); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("work\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Tool: antigravity") {
		t.Fatalf("the continuing conversation did not claim its own commit:\n%s", data)
	}
}

func TestAntigravityHandlersIgnoreBadInputAndStillAck(t *testing.T) {
	stateDir := t.TempDir()
	ctx := context.Background()
	t.Setenv(antigravityConversationEnv, "")
	for _, stdin := range []string{"", "not json", `{"modelName":"x"}`, `{"conversationId":"../../etc"}`, `{"conversationId":"c1","toolCall":{"name":"write_to_file","args":"not-an-object"}}`} {
		for name, h := range map[string]func(context.Context, hookrun.Env) error{
			"pre": preInvocation, semconv.GenAIMainAgentNameKey: postToolUse, "post": postInvocation, "stop": stop,
		} {
			var out bytes.Buffer
			if err := h(ctx, hookrun.Env{StateDir: stateDir, Cwd: t.TempDir(), Stdin: strings.NewReader(stdin), Stdout: &out}); err != nil {
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
	root := hookruntest.InitRepo(t)
	sp, _ := spool.Open(t.TempDir())
	stateDir := t.TempDir()
	t.Setenv(antigravityConversationEnv, "env-conv-1")
	stdin := `{"workspacePaths":["` + root + `"],"modelName":"gemini-3.8-pro","initialNumSteps":1,"invocationNum":0}`
	if err := preInvocation(context.Background(), hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	events := hookruntest.Spooled(t, sp)
	if len(events) != 2 || events[0].Name != semconv.TermaSessionStartEvent || events[1].Name != semconv.TermaSessionObservationEvent {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events {
		if e.SessionID != "env-conv-1" {
			t.Fatalf("event outside the conversation: %+v", e)
		}
	}
}

// A turn is named by where it began, from a recording of two turns of one resumed
// conversation.
func TestAntigravityTurnsAreNamedByWhereTheyBegan(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	stateDir := t.TempDir()
	run := func(h func(context.Context, hookrun.Env) error, extra string) {
		t.Helper()
		if err := h(ctx, hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(antigravityPayload(root, "", extra)), Spool: sp, Version: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	// A step before terma has seen any turn begin: reported, and placed in no turn.
	run(postToolUse, `"error":"","stepIdx":0,"toolCall":{"args":{},"name":"view_file"}`)

	run(preInvocation, `"initialNumSteps":1,"invocationNum":0`)
	run(postToolUse, `"error":"","stepIdx":2,"toolCall":{"args":{"CommandLine":"ls missing"},"name":"run_command"}`)
	run(postInvocation, `"initialNumSteps":1,"invocationNum":0`)
	run(preInvocation, `"initialNumSteps":3,"invocationNum":1`)
	run(postInvocation, `"initialNumSteps":3,"invocationNum":1`)
	run(stop, `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)

	run(preInvocation, `"initialNumSteps":9,"invocationNum":0`)
	run(postToolUse, `"error":"tool crashed","stepIdx":11,"toolCall":{"args":{},"name":"view_file"}`)
	run(postInvocation, `"initialNumSteps":9,"invocationNum":0`)
	run(stop, `"error":"","executionNum":0,"fullyIdle":true,"terminationReason":"NO_TOOL_CALL"`)

	var got []string
	for _, e := range hookruntest.Spooled(t, sp) {
		turn, _ := e.Attrs[semconv.TermaTurnIDKey].(string)
		switch e.Name {
		case semconv.TermaToolCallEvent:
			got = append(got, "call "+e.Attrs[semconv.GenAIToolCallIDKey].(string)+" "+e.Attrs[semconv.TermaOperationStatusKey].(string)+" "+turn)
			if _, ok := e.Attrs["error"]; ok {
				t.Error("a tool's error text must not travel")
			}
		case semconv.TermaSessionObservationEvent:
			got = append(got, e.Attrs[semconv.TermaHookEventKey].(string)+" "+turn)
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
	t.Parallel()
	if _, ok := antigravityToolCallAttrs(&antigravityHookInput{}, "turn-1"); ok {
		t.Fatal("a payload with no tool and no step made a tool call")
	}
	a, ok := antigravityToolCallAttrs(&antigravityHookInput{StepIdx: []byte("5")}, "")
	if !ok || a[semconv.GenAIToolCallIDKey] != "step-5" || a[semconv.TermaStepIndexKey] != int64(5) {
		t.Fatalf("a step alone identifies a call: %v %v", a, ok)
	}
	if _, has := a[semconv.TermaTurnIDKey]; has {
		t.Fatalf("an unknown turn stays missing: %v", a)
	}
}

func TestAntigravityEditedPaths(t *testing.T) {
	t.Parallel()
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

// The reader refuses a payload past the bound by name.
func TestReaderRefusesOversizedInput(t *testing.T) {
	t.Setenv(antigravityConversationEnv, "")
	if _, err := readAntigravityInput(strings.NewReader(`{"conversationId":"valid"}`)); err != nil {
		t.Fatalf("a payload within the bound was refused: %v", err)
	}
	pad := strings.Repeat(" ", hookrun.MaxInput)
	if _, err := readAntigravityInput(strings.NewReader(`{"conversationId":"valid"}` + pad)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized payload: err = %v, want too large", err)
	}
}
