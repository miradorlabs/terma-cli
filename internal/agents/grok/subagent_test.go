package grok_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The sessions and calls of headless_subagents_foreground_awaited_killed: the parent ran a
// foreground subagent, then a background one it waited on, whose launch hook came before its
// SubagentStart, then a background one it killed, whose SubagentStart came first.
const (
	toolsParent     = "01a20e97-9052-7770-a911-1b8197e5884f"
	foregroundChild = "01a20e97-a2a1-7d51-a63e-1bc7006577fa"
	awaitedChild    = "01a20e97-c18a-7d42-a098-3b74de30ac2d"
	killedChild     = "01a20e97-e64e-7d22-96ff-e37a8df9d0dd"
	foregroundCall  = "call-b9df9c1e-c88c-4ecc-8c56-a2263fd217f7-0"
	awaitedCall     = "call-578f88ab-04b3-45a4-b894-7448cf27e299-1"
	killedCall      = "call-1bd7eb95-650d-4e47-a26c-55cc90783ba5-3"

	subagentsFixture = "headless_subagents_foreground_awaited_killed"
)

// subagentRecord is what a test expects of one terma.subagent.call or terma.subagent.end: an
// empty kind, call id or duration means the attribute is absent. The duration is the number as
// text, so a record that carries a zero is told from one that carries none.
type subagentRecord struct {
	agent, kind, call, status, durationMs string
}

func subagentRecords(t *testing.T, sp *spool.Spool) (calls, ends []subagentRecord) {
	t.Helper()
	events := hookruntest.Spooled(t, sp)
	read := func(name string) []subagentRecord {
		var out []subagentRecord
		for _, e := range hookruntest.Named(events, name) {
			if e.SessionID != toolsParent || e.Attrs[semconv.GenAIMainAgentNameKey] != "grok" {
				t.Errorf("%s filed under %s as %v", name, e.SessionID, e.Attrs[semconv.GenAIMainAgentNameKey])
			}
			for key := range e.Attrs {
				switch key {
				case semconv.GenAIMainAgentNameKey, semconv.TermaAgentIDKey, semconv.GenAIAgentNameKey, semconv.GenAIToolCallIDKey,
					semconv.TermaOperationStatusKey, semconv.TermaOperationDurationMsKey, "project_id":
				default:
					t.Errorf("%s carries %s = %v", name, key, e.Attrs[key])
				}
			}
			r := subagentRecord{status: str(e.Attrs[semconv.TermaOperationStatusKey]), agent: str(e.Attrs[semconv.TermaAgentIDKey]),
				kind: str(e.Attrs[semconv.GenAIAgentNameKey]), call: str(e.Attrs[semconv.GenAIToolCallIDKey])}
			if d, ok := e.Attrs[semconv.TermaOperationDurationMsKey]; ok {
				r.durationMs = strconv.FormatFloat(hookruntest.Num(d), 'f', -1, 64)
			}
			out = append(out, r)
		}
		return out
	}
	return read(semconv.TermaSubagentCallEvent), read(semconv.TermaSubagentEndEvent)
}

func str(v any) string { s, _ := v.(string); return s }

func sameRecords(t *testing.T, what string, got, want []subagentRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %+v, want %+v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s %d: %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

var (
	foregroundCallRecord = subagentRecord{agent: foregroundChild, kind: "general-purpose", call: foregroundCall, status: "completed", durationMs: "3004"}
	awaitedCallRecord    = subagentRecord{agent: awaitedChild, call: awaitedCall, status: "async_launched"}
	killedCallRecord     = subagentRecord{agent: killedChild, call: killedCall, status: "async_launched"}
	awaitedEndRecord     = subagentRecord{agent: awaitedChild, status: "completed", durationMs: "4080"}
	killedEndRecord      = subagentRecord{agent: killedChild, status: "aborted"}
)

// The parent's spawn_subagent is the subagent's launching call: a foreground one completed,
// with its kind and how long it ran; a background one async_launched, whichever came first,
// its launch hook or its SubagentStart. Waiting on a background subagent that completed ends
// it completed, with grok's duration in milliseconds, and killing one ends it aborted; neither
// names a call, since neither launched it.
func TestCapturedSubagentToolsReportTheRuns(t *testing.T) {
	run := replay(t, subagentsFixture, replayOpts{})
	calls, ends := subagentRecords(t, run.sp)
	sameRecords(t, "calls", calls, []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord})
	sameRecords(t, "ends", ends, []subagentRecord{awaitedEndRecord, killedEndRecord})
}

// editTool rewrites the post_tool_use payloads of tool with edit.
func editTool(tool string, edit func(m map[string]any)) func(string, []byte) []byte {
	return func(event string, payload []byte) []byte {
		if event != "post_tool_use" {
			return payload
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil || m["tool_name"] != tool {
			return payload
		}
		edit(m)
		out, _ := json.Marshal(m)
		return out
	}
}

// setResponse sets a payload's tool result, in both of grok's keys.
func setResponse(m map[string]any, result string) {
	var v any
	_ = json.Unmarshal([]byte(result), &v)
	m["tool_response"], m["toolResult"] = v, v
}

// A background shell command's wait and kill have a subagent's shape: only a task terma saw
// the session launch as a subagent ends one. The wait's result is the capture
// headless_background_command's, cut to what terma reads.
func TestShellTasksEndNoSubagent(t *testing.T) {
	const shellTask = "97460317-abd9-43f1-9c4a-9a96e412bf07"
	shellOutput := `{"type":"TaskOutput","Result":{"task_id":"` + shellTask + `","command":"sleep 3; echo bg-done","status":"completed","exit_code":0,` +
		`"started":"2026-11-23T20:07:31Z","ended":"2026-11-23T20:07:34Z","duration_secs":3.006446538,"output":"bg-done\n","truncated":false}}`
	shellKill := `{"type":"KillTask","Result":{"task_id":"` + shellTask + `","outcome":"killed","message":"Task killed"}}`
	edit := func(event string, payload []byte) []byte {
		payload = editTool("get_command_or_subagent_output", func(m map[string]any) {
			m["tool_input"] = map[string]any{"task_ids": []string{shellTask}, "timeout_ms": 10000}
			setResponse(m, shellOutput)
		})(event, payload)
		return editTool("kill_command_or_subagent", func(m map[string]any) {
			m["tool_input"] = map[string]any{"task_id": shellTask}
			setResponse(m, shellKill)
		})(event, payload)
	}
	run := replay(t, subagentsFixture, replayOpts{edit: edit})
	calls, ends := subagentRecords(t, run.sp)
	sameRecords(t, "calls", calls, []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord})
	sameRecords(t, "ends", ends, nil)
}

// What the parent's subagent tools report beyond what grok was seen to send: nothing a result
// does not say, and unknown for a result naming a subagent in a shape not seen.
func TestSubagentToolResultsSendOnlyWhatTheySay(t *testing.T) {
	withText := func(text string) func(m map[string]any) {
		return func(m map[string]any) {
			if str(m["tool_use_id"]) == awaitedCall {
				b, _ := json.Marshal(map[string]any{"type": "Text", "text": text})
				setResponse(m, string(b))
			}
		}
	}
	for name, c := range map[string]struct {
		edit        func(string, []byte) []byte
		calls, ends []subagentRecord
	}{
		// The awaited subagent's launch names it nowhere else; its SubagentStart, which came
		// after, still makes it known, so the wait still ends it.
		"background text without the id line": {
			edit:  editTool("spawn_subagent", withText("Subagent started in background.\ndescription: Read a.txt content")),
			calls: []subagentRecord{foregroundCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		// Only grok's own two lines are read: an id anywhere else in the text, the description
		// the model wrote included, names nobody.
		"text naming a subagent but not opening with grok's line": {
			edit:  editTool("spawn_subagent", withText("Done.\nsubagent_id: "+awaitedChild)),
			calls: []subagentRecord{foregroundCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		"a description naming another subagent": {
			edit: editTool("spawn_subagent", withText("Subagent started in background.\nsubagent_id: "+awaitedChild+
				"\ndescription: two lines\nsubagent_id: "+killedChild)),
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		"an id of another shape": {
			edit:  editTool("spawn_subagent", withText("Subagent started in background.\nsubagent_id: secret words\ndescription: x")),
			calls: []subagentRecord{foregroundCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		// The text alone says a launch went to the background: the input's flag is not read.
		"a background launch whose input does not say so": {
			edit: editTool("spawn_subagent", func(m map[string]any) {
				if in, _ := m["tool_input"].(map[string]any); in != nil {
					delete(in, "background")
				}
			}),
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		// The launch hook alone makes a subagent known: here its SubagentStart names nobody.
		"a wait on a subagent only its launch hook named": {
			edit: func(event string, payload []byte) []byte {
				if event != "subagent_start" || !strings.Contains(string(payload), awaitedChild) {
					return payload
				}
				return []byte(strings.ReplaceAll(string(payload), awaitedChild, ""))
			},
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		// completed with an exit code that is not zero, or with none, is not taken for a success.
		"a wait on a run that completed with a failing exit code": {
			edit: editTool("get_command_or_subagent_output", func(m map[string]any) {
				setResponse(m, `{"type":"TaskOutput","Result":{"task_id":"`+awaitedChild+`","status":"completed","exit_code":1,"duration_secs":4.08}}`)
			}),
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{killedEndRecord},
		},
		"a wait on a run that completed with no exit code": {
			edit: editTool("get_command_or_subagent_output", func(m map[string]any) {
				setResponse(m, `{"type":"TaskOutput","Result":{"task_id":"`+awaitedChild+`","status":"completed","duration_secs":4.08}}`)
			}),
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{killedEndRecord},
		},
		"a child's own launch": {
			edit: editTool("spawn_subagent", func(m map[string]any) {
				if str(m["tool_use_id"]) == foregroundCall {
					m["subagentType"] = "general-purpose"
				}
			}),
			calls: []subagentRecord{awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		"a result of a type not seen": {
			edit: editTool("spawn_subagent", func(m map[string]any) {
				if str(m["tool_use_id"]) == foregroundCall {
					setResponse(m, `{"type":"SubagentFailed","subagent_id":"`+foregroundChild+`","subagent_type":"general-purpose","duration_ms":3004}`)
				}
			}),
			calls: []subagentRecord{{agent: foregroundChild, kind: "general-purpose", call: foregroundCall, status: "unknown"}, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		"a foreground run without a duration": {
			edit: editTool("spawn_subagent", func(m map[string]any) {
				if str(m["tool_use_id"]) == foregroundCall {
					setResponse(m, `{"type":"SubagentCompleted","subagent_id":"`+foregroundChild+`","subagent_type":"general-purpose"}`)
				}
			}),
			calls: []subagentRecord{{agent: foregroundChild, kind: "general-purpose", call: foregroundCall, status: "completed"}, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, killedEndRecord},
		},
		"a wait on a run not completed, and a kill not done": {
			edit: func(event string, payload []byte) []byte {
				payload = editTool("get_command_or_subagent_output", func(m map[string]any) {
					setResponse(m, `{"type":"TaskOutput","Result":{"task_id":"`+awaitedChild+`","status":"running","duration_secs":1}}`)
				})(event, payload)
				return editTool("kill_command_or_subagent", func(m map[string]any) {
					setResponse(m, `{"type":"KillTask","Result":{"task_id":"`+killedChild+`","outcome":"not_found"}}`)
				})(event, payload)
			},
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
		},
		"a wait on several": {
			edit: editTool("get_command_or_subagent_output", func(m map[string]any) {
				setResponse(m, `{"type":"TaskOutput","Result":[{"task_id":"`+awaitedChild+`","status":"completed","exit_code":0,"duration_secs":4.08},`+
					`{"task_id":"`+foregroundChild+`","status":"completed","exit_code":0},{"task_id":"97460317-abd9-43f1-9c4a-9a96e412bf07","status":"completed","exit_code":0}]}`)
			}),
			calls: []subagentRecord{foregroundCallRecord, awaitedCallRecord, killedCallRecord},
			ends:  []subagentRecord{awaitedEndRecord, {agent: foregroundChild, status: "completed"}, killedEndRecord},
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := replay(t, subagentsFixture, replayOpts{edit: c.edit})
			calls, ends := subagentRecords(t, run.sp)
			sameRecords(t, "calls", calls, c.calls)
			sameRecords(t, "ends", ends, c.ends)
		})
	}
}
