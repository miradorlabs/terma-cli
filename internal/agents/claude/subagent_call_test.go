package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The payloads below are reduced live payloads whose description, prompt and reply are sentinels,
// so a test can say none of it reaches the spool.
const (
	sentinelDescription = "NEVER-EXPORT-DESCRIPTION"
	sentinelPrompt      = "NEVER-EXPORT-PROMPT"
	sentinelReply       = "NEVER-EXPORT-REPLY"
)

func agentToolPayload(cwd, toolName, response string, extra string) string {
	return `{"session_id":"sess-agent-1","cwd":` + jsonString(cwd) + `,"prompt_id":"prompt-7","permission_mode":"default",` + extra +
		`"hook_event_name":"PostToolUse","tool_name":"` + toolName + `",` +
		`"tool_input":{"description":"` + sentinelDescription + `","prompt":"` + sentinelPrompt + `","subagent_type":"general-purpose","run_in_background":false},` +
		`"tool_response":` + response + `,"tool_use_id":"toolu_014qGq4PjQrSYtjabLXYacte","duration_ms":6313}`
}

const completedAgentResponse = `{"status":"completed","prompt":"` + sentinelPrompt + `","agentId":"acb384f0c825d61ec","agentType":"general-purpose",` +
	`"harnessNoteCount":0,"content":[{"type":"text","text":"` + sentinelReply + `"}],"resolvedModel":"claude-haiku-4-5-20251001",` +
	`"totalDurationMs":6311,"totalTokens":15385,"totalToolUseCount":1,` +
	`"usage":{"output_tokens_details":{"thinking_tokens":42},"input_tokens":8,"cache_creation_input_tokens":2192,"cache_read_input_tokens":13060,"output_tokens":125,"service_tier":"standard","speed":"standard"},` +
	`"toolStats":{"readCount":0,"searchCount":0,"bashCount":0,"editFileCount":1,"linesAdded":1,"linesRemoved":0,"otherToolCount":0}}`

const launchedAgentResponse = `{"isAsync":true,"status":"async_launched","agentId":"a838402ff1ef43bee","description":"` + sentinelDescription + `",` +
	`"resolvedModel":"claude-haiku-4-5-20251001","prompt":"` + sentinelPrompt + `","outputFile":"/private/tmp/x/tasks/a838402ff1ef43bee.output","canReadOutputFile":true}`

func runPostToolUse(t *testing.T, root string, sp *spool.Spool, payload string) {
	stateDir := t.TempDir()
	t.Helper()
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(payload), Spool: sp, Version: "test"}
	if err := postToolUse(context.Background(), env); err != nil {
		t.Fatalf("a hook must never fail: %v", err)
	}
}

// A waited-for subagent's Agent tool response names its model and how the run went.
func TestClaudeSubagentCallCarriesTheModelAndTheRollUp(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", completedAgentResponse, ""))

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if len(events) != 1 || events[0].Name != semconv.TermaSubagentCallEvent {
		t.Fatalf("events = %s, want one %s", hookruntest.Names(events), semconv.TermaSubagentCallEvent)
	}
	ev := events[0]
	if ev.SessionID != "sess-agent-1" {
		t.Errorf("filed under %q, want the parent session", ev.SessionID)
	}
	for key, want := range map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaAgentIDKey: "acb384f0c825d61ec", semconv.GenAIAgentNameKey: "general-purpose",
		semconv.GenAIRequestModelKey: "claude-haiku-4-5-20251001", semconv.GenAIToolCallIDKey: "toolu_014qGq4PjQrSYtjabLXYacte",
		semconv.TermaTurnIDKey: "prompt-7", semconv.TermaOperationStatusKey: "completed",
	} {
		if ev.Attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, ev.Attrs[key], want)
		}
	}
	if got := hookruntest.Num(ev.Attrs[semconv.TermaOperationDurationMsKey]); got != 6311 {
		t.Errorf("duration = %v, want 6311", got)
	}
	// The run's roll-up and the last request's usage are never sent.
	for _, key := range []string{"service_tier", "speed", "final_context_tokens", "tool_call_count", "edit_file_count", "lines_added", "lines_removed", "read_count", "total_tokens", "input_tokens", "output_tokens", "is_async"} {
		if got, ok := ev.Attrs[key]; ok {
			t.Errorf("%s = %v is sent", key, got)
		}
	}
	if _, ok := ev.Attrs[semconv.TermaAgentParentIDKey]; ok {
		t.Error("a subagent launched by the main thread has no parent agent")
	}
	if manifests, _ := hookruntest.Store(t, root).Manifests(); len(manifests) != 0 {
		t.Errorf("an Agent call built a manifest: %+v", manifests)
	}
}

// A background launch has no totals, and they stay absent: a zero would read as nothing used.
func TestClaudeSubagentCallLaunchedInTheBackgroundHasNoTotals(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", launchedAgentResponse, ""))

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if len(events) != 1 {
		t.Fatalf("events = %s", hookruntest.Names(events))
	}
	ev := events[0]
	if ev.Attrs[semconv.TermaOperationStatusKey] != "async_launched" || ev.Attrs[semconv.GenAIRequestModelKey] != "claude-haiku-4-5-20251001" {
		t.Errorf("launch record: %+v", ev.Attrs)
	}
	for _, key := range []string{semconv.TermaOperationDurationMsKey} {
		if _, ok := ev.Attrs[key]; ok {
			t.Errorf("%s was not reported and must be absent, got %v", key, ev.Attrs[key])
		}
	}
}

// No field of claudeAgentResult decodes the task's prompt or the reply.
func TestClaudeSubagentCallNeverCarriesWhatWasSaid(t *testing.T) {
	root := newRepo(t)
	for _, response := range []string{completedAgentResponse, launchedAgentResponse} {
		sp, _ := spool.Open(t.TempDir())
		runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", response, ""))
		raw, err := json.Marshal(hookruntest.Spooled(t, sp))
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range []string{sentinelDescription, sentinelPrompt, sentinelReply} {
			if strings.Contains(string(raw), sentinel) {
				t.Fatalf("%s reached the spool: %s", sentinel, raw)
			}
		}
	}
}

func TestClaudeSubagentCallVariants(t *testing.T) {
	root := newRepo(t)
	for _, tc := range []struct {
		name, tool, response, extra string
		check                       func(*testing.T, []spool.Event)
	}{
		{"the tool was called Task before it was Agent", "Task", completedAgentResponse, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[semconv.TermaAgentIDKey] != "acb384f0c825d61ec" {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a subagent that launches another is its parent", "Agent", completedAgentResponse, `"agent_id":"parent-agent-9","agent_type":"planner",`, func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[semconv.TermaAgentParentIDKey] != "parent-agent-9" || evs[0].Attrs[semconv.TermaAgentIDKey] != "acb384f0c825d61ec" {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a status outside the vocabulary arrives as unknown, not as free text", "Agent", `{"status":"something new <script>","agentId":"a1"}`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[semconv.TermaOperationStatusKey] != hookrun.UnknownValue {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a number that is not a count is dropped, not coerced", "Agent", `{"status":"completed","agentId":"a1","totalTokens":-5,"totalToolUseCount":1.5,"totalDurationMs":"soon"}`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 {
				t.Fatalf("events = %+v", evs)
			}
			for _, key := range []string{"final_context_tokens", "tool_call_count", "duration_ms"} {
				if _, ok := evs[0].Attrs[key]; ok {
					t.Errorf("%s = %v should have been dropped", key, evs[0].Attrs[key])
				}
			}
		}},
		{"a response that names no agent is nothing to record", "Agent", `{"status":"completed"}`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 0 {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"an unsafe agent id is refused", "Agent", `{"status":"completed","agentId":"../../etc/passwd"}`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 0 {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a response that is not an object is nothing to record", "Agent", `"a string"`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 0 {
				t.Errorf("events = %+v", evs)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, _ := spool.Open(t.TempDir())
			runPostToolUse(t, root, sp, agentToolPayload(root, tc.tool, tc.response, tc.extra))
			tc.check(t, hookruntest.Lifecycle(hookruntest.Spooled(t, sp)))
		})
	}
}

// An edit names its tool call, so the platform can fold it into Claude's own record of that call.
func TestEditNamesItsToolCall(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	hookruntest.WriteFile(t, root, "src/main.go", "package src\n")
	runPostToolUse(t, root, sp, `{"session_id":"sess-edit-1","cwd":`+jsonString(root)+`,"hook_event_name":"PostToolUse","tool_name":"Edit","tool_use_id":"toolu_01","tool_input":{"file_path":"src/main.go"}}`)

	events := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaFilesTouchedEvent)
	if len(events) != 1 || events[0].Attrs[semconv.GenAIToolCallIDKey] != "toolu_01" {
		t.Fatalf("touched = %+v", events)
	}
}

// `claude --agent <name>` stamps agent_type on every hook; without agent_id it is not a subagent.
func TestAgentTypeWithoutAnAgentIDIsNotASubagent(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	hookruntest.WriteFile(t, root, "src/main.go", "package src\n")
	runPostToolUse(t, root, sp, `{"session_id":"sess-agent-2","cwd":`+jsonString(root)+`,"agent_type":"reviewer","hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"src/main.go"}}`)

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if len(events) != 1 || events[0].Name != semconv.TermaFilesTouchedEvent {
		t.Fatalf("events = %s", hookruntest.Names(events))
	}
	for _, key := range []string{semconv.TermaAgentIDKey, semconv.GenAIAgentNameKey} {
		if v, ok := events[0].Attrs[key]; ok {
			t.Errorf("%s = %v on a main-thread edit", key, v)
		}
	}
}
