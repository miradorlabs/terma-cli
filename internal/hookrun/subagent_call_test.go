package hookrun

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The payloads below are Claude Code 2.1.278's, captured live on 2026-09-21 and reduced:
// ids shortened, and the three places conversation content appears — the task's
// description, its prompt, the subagent's reply — replaced with sentinels, so a test can
// say that none of it reaches the spool.
const (
	sentinelDescription = "NEVER-EXPORT-DESCRIPTION"
	sentinelPrompt      = "NEVER-EXPORT-PROMPT"
	sentinelReply       = "NEVER-EXPORT-REPLY"
)

func agentToolPayload(cwd, toolName, response string, extra string) string {
	return `{"session_id":"sess-agent-1","cwd":` + quoteJSON(cwd) + `,"prompt_id":"prompt-7","permission_mode":"default",` + extra +
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
	t.Helper()
	env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(payload), Spool: sp, Version: "test"}
	if err := PostToolUse(context.Background(), env); err != nil {
		t.Fatalf("a hook must never fail: %v", err)
	}
}

// A subagent the parent waited for: the Agent tool's response is the one hook that names
// the subagent's model, and the only record of what it used that is keyed to the agent.
func TestClaudeSubagentCallCarriesTheModelAndTheRollUp(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", completedAgentResponse, ""))

	events := lifecycle(drain(t, sp))
	if len(events) != 1 || events[0].Name != EventSubagentCall {
		t.Fatalf("events = %s, want one %s", names(events), EventSubagentCall)
	}
	ev := events[0]
	if ev.SessionID != "sess-agent-1" {
		t.Errorf("filed under %q, want the parent session", ev.SessionID)
	}
	for key, want := range map[string]any{
		attrTool: claudeTool, attrAgentID: "acb384f0c825d61ec", attrAgentType: "general-purpose",
		attrModel: "claude-haiku-4-5-20251001", attrToolCallID: "toolu_014qGq4PjQrSYtjabLXYacte",
		attrTurnID: "prompt-7", attrStatus: "completed", "service_tier": "standard", "speed": "standard",
	} {
		if ev.Attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, ev.Attrs[key], want)
		}
	}
	for key, want := range map[string]float64{
		"duration_ms": 6311, "final_context_tokens": 15385, "tool_call_count": 1,
		"edit_file_count": 1, "lines_added": 1, "lines_removed": 0, "read_count": 0,
	} {
		if got, ok := ev.Attrs[key]; !ok || num(got) != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, want)
		}
	}
	// The response's `usage` is the subagent's LAST request, not the run: this fixture's run made two
	// (input / cache read / cache write 10/0/13060, then 8/13060/2192), and usage is the second. Sent on
	// beside the run's duration those numbers read as what the run spent — and were taken for it. The
	// native export already carries that request under its own id, so they are not sent at all.
	for _, key := range []string{"total_tokens", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "thinking_tokens"} {
		if got, ok := ev.Attrs[key]; ok {
			t.Errorf("%s = %v: one request's usage must never travel as a run's", key, got)
		}
	}
	if _, ok := ev.Attrs["is_async"]; ok {
		t.Error("is_async was not in the payload and must not be invented")
	}
	if _, ok := ev.Attrs[attrAgentParentID]; ok {
		t.Error("a subagent launched by the main thread has no parent agent")
	}
	// The manifest is for edits. Launching a subagent touches no file.
	if manifests, _ := openRepoStore(t, root).Manifests(); len(manifests) != 0 {
		t.Errorf("an Agent call built a manifest: %+v", manifests)
	}
}

// Launched in the background, the call returns at once: the id and the model, and no
// totals. They must stay absent — a zero would read as a subagent that used nothing.
func TestClaudeSubagentCallLaunchedInTheBackgroundHasNoTotals(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", launchedAgentResponse, ""))

	events := lifecycle(drain(t, sp))
	if len(events) != 1 {
		t.Fatalf("events = %s", names(events))
	}
	ev := events[0]
	if ev.Attrs[attrStatus] != "async_launched" || ev.Attrs["is_async"] != true || ev.Attrs[attrModel] != "claude-haiku-4-5-20251001" {
		t.Errorf("launch record: %+v", ev.Attrs)
	}
	for _, key := range []string{"duration_ms", "final_context_tokens", "tool_call_count", "lines_added"} {
		if _, ok := ev.Attrs[key]; ok {
			t.Errorf("%s was not reported and must be absent, got %v", key, ev.Attrs[key])
		}
	}
}

// The response holds the task's prompt and the subagent's reply. No field of
// claudeAgentResult decodes them, and this is what would notice one being added.
func TestClaudeSubagentCallNeverCarriesWhatWasSaid(t *testing.T) {
	root := initRepo(t)
	for _, response := range []string{completedAgentResponse, launchedAgentResponse} {
		sp, _ := spool.Open(t.TempDir())
		runPostToolUse(t, root, sp, agentToolPayload(root, "Agent", response, ""))
		raw, err := json.Marshal(drain(t, sp))
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
	root := initRepo(t)
	for _, tc := range []struct {
		name, tool, response, extra string
		check                       func(*testing.T, []spool.Event)
	}{
		{"the tool was called Task before it was Agent", "Task", completedAgentResponse, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[attrAgentID] != "acb384f0c825d61ec" {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a subagent that launches another is its parent", "Agent", completedAgentResponse, `"agent_id":"parent-agent-9","agent_type":"planner",`, func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[attrAgentParentID] != "parent-agent-9" || evs[0].Attrs[attrAgentID] != "acb384f0c825d61ec" {
				t.Errorf("events = %+v", evs)
			}
		}},
		{"a status outside the vocabulary arrives as unknown, not as free text", "Agent", `{"status":"something new <script>","agentId":"a1"}`, "", func(t *testing.T, evs []spool.Event) {
			if len(evs) != 1 || evs[0].Attrs[attrStatus] != unknownValue {
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
			tc.check(t, lifecycle(drain(t, sp)))
		})
	}
}

// openRepoStore is the session store of a test repository: what a hook wrote there.
func openRepoStore(t *testing.T, root string) *session.Store {
	t.Helper()
	return session.Open(filepath.Join(root, ".git"))
}

// `claude --agent <name>` stamps agent_type on every hook of the session, the main
// thread's included. Only agent_id says a hook fired inside a subagent, so a type
// without one must not make an edit look like a subagent's.
func TestAgentTypeWithoutAnAgentIDIsNotASubagent(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	writeFile(t, root, "src/main.go", "package src\n")
	runPostToolUse(t, root, sp, `{"session_id":"sess-agent-2","cwd":`+quoteJSON(root)+`,"agent_type":"reviewer","hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"src/main.go"}}`)

	events := lifecycle(drain(t, sp))
	if len(events) != 1 || events[0].Name != EventFilesTouched {
		t.Fatalf("events = %s", names(events))
	}
	for _, key := range []string{attrAgentID, attrAgentType} {
		if v, ok := events[0].Attrs[key]; ok {
			t.Errorf("%s = %v on a main-thread edit", key, v)
		}
	}
}
