package grok

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// subagentDir holds the evidence that a session launched a subagent, per (session, child), so
// a task id an awaiting or killing call names is known to be one: a background shell command's
// result has the same shape.
var subagentDir = hookrun.AgentStateDir(name, "subagents")

// The tools whose PostToolUse in the parent reports a subagent: its launch, a wait on a
// background one, and a kill of one. The last two take shell tasks too.
const (
	toolSpawnSubagent = "spawn_subagent"
	toolTaskOutput    = "get_command_or_subagent_output"
	toolKillTask      = "kill_command_or_subagent"
)

// subagentStart announces the subagent grok is about to run as a session of its own. grok
// fires it in the parent: session_id is the parent and subagentId the child, whose first
// record and first hook come after it. It claims the child with the parent named, for the
// relay to stamp on the child's records (grok's export links nothing), and spools the child's
// start with its parent and kind. The child is never made active. It also records the launch,
// for the parent's later calls on the child.
func subagentStart(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.SubagentID) || in.SubagentID == in.SessionID {
		return nil
	}
	r, ok := env.Open(ctx, in.SubagentID, in.Cwd)
	if !ok {
		return nil
	}
	env.RememberSubagent(subagentDir, in.SessionID, in.SubagentID)
	var extra map[string]any
	if hookrun.ShortLabel(in.SubagentType) {
		extra = map[string]any{semconv.GenAIAgentNameKey: in.SubagentType}
	}
	env.StartSubagent(r, env.NewSession(r, in.SubagentID, name, ""), in.SessionID, true, extra)
	return nil
}

// subagentTool records what the parent's spawn_subagent, get_command_or_subagent_output or
// kill_command_or_subagent says of a subagent, as Claude Code's Agent tool and SubagentStop
// do: the launching call (terma.subagent.call) and, when the parent waits on or kills a
// background subagent, how it ended (terma.subagent.end). Each record carries ids, a kind, a
// status and a duration, never what was said. A subagent's own call is not handled: its
// hooks name no launching agent, so it sends nothing. A background subagent nobody waits on
// or kills sends no end: no hook says how it ended.
func subagentTool(ctx context.Context, env hookrun.Env, in *grokHookInput) {
	if in.child() || !session.ValidID(in.SessionID) {
		return
	}
	var events []spool.Event
	var launched string
	switch in.ToolName {
	case toolSpawnSubagent:
		if ev, child, ok := subagentCall(env, in); ok {
			events, launched = append(events, ev), child
		}
	case toolTaskOutput:
		events = subagentEnds(env, in, "TaskOutput")
	case toolKillTask:
		events = subagentEnds(env, in, "KillTask")
	}
	if len(events) == 0 {
		return
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return
	}
	// The launch hook can come before the child's SubagentStart, so it records the launch too.
	if launched != "" {
		env.RememberSubagent(subagentDir, in.SessionID, launched)
	}
	for _, ev := range events {
		env.EmitFor(r, ev)
	}
}

// spawnResult is spawn_subagent's result as far as terma reads it: a foreground run's
// {"type":"SubagentCompleted","subagent_id":…,"subagent_type":…,"duration_ms":…}, or a
// background launch's {"type":"Text","text":…}, whose text names the child on a line of its
// own. The output and description have no field, so they are never decoded.
type spawnResult struct {
	Type         string          `json:"type"`
	SubagentID   string          `json:"subagent_id"`
	SubagentType string          `json:"subagent_type"`
	DurationMs   json.RawMessage `json:"duration_ms"`
	Text         string          `json:"text"`
}

// subagentCall is the terma.subagent.call for a spawn_subagent: completed, with its kind and
// how long it ran, for a foreground run, which grok reports once the child finished;
// async_launched for a background launch; unknown for any other result naming a child.
func subagentCall(env hookrun.Env, in *grokHookInput) (ev spool.Event, child string, ok bool) {
	var res spawnResult
	if len(in.ToolResponse) == 0 || json.Unmarshal(in.ToolResponse, &res) != nil {
		env.Logf("%s without a readable result", in.ToolName)
		return spool.Event{}, "", false
	}
	status, child := hookrun.UnknownValue, res.SubagentID
	switch {
	case res.Type == "SubagentCompleted":
		status = "completed"
	case res.Type == "Text" && background(in.ToolInput.Background):
		status, child = "async_launched", textSubagentID(res.Text)
		if child == "" {
			env.Logf("%s background result names no subagent", in.ToolName)
			return spool.Event{}, "", false
		}
	}
	if !session.ValidID(child) || child == in.SessionID {
		env.Logf("%s result names no subagent", in.ToolName)
		return spool.Event{}, "", false
	}
	kind := res.SubagentType
	if status == "async_launched" {
		kind = ""
	}
	attrs := hookrun.AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: name}, child, kind)
	hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolUseID)
	attrs[semconv.TermaOperationStatusKey] = status
	if status == "completed" {
		if value, _, ok := hookrun.JSONNumber(res.DurationMs, true); ok {
			attrs[semconv.TermaOperationDurationMsKey] = int64(value)
		}
	}
	return spool.Event{Name: semconv.TermaSubagentCallEvent, SessionID: in.SessionID, Attrs: attrs}, child, true
}

// background reports whether spawn_subagent's background argument is true.
func background(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "true" }

// textSubagentID is the child a background launch's text names, on the one line
// "subagent_id: <id>"; "" when no line, or more than one, does.
func textSubagentID(text string) string {
	var id string
	for line := range strings.SplitSeq(text, "\n") {
		v, ok := strings.CutPrefix(line, "subagent_id: ")
		if !ok {
			continue
		}
		if id != "" || !session.ValidID(v) {
			return ""
		}
		id = v
	}
	return id
}

// taskResult is one task in get_command_or_subagent_output's or kill_command_or_subagent's
// result, as far as terma reads it; the command and output have no field.
type taskResult struct {
	TaskID       string          `json:"task_id"`
	Status       string          `json:"status"`
	DurationSecs json.RawMessage `json:"duration_secs"`
	Outcome      string          `json:"outcome"`
}

// subagentEnds are the terma.subagent.end records a wait on or a kill of tasks reports, one
// for each task terma saw this session launch as a subagent: a wait's
// {"type":"TaskOutput","Result":{…}} (or, not yet seen, a list of them) whose status is
// completed, and a kill's {"type":"KillTask","Result":{…}} whose outcome is killed, which
// ends it aborted. No other status is read: grok's words for a failure have not been seen. The
// record names no tool call: this one did not launch the subagent.
func subagentEnds(env hookrun.Env, in *grokHookInput, want string) []spool.Event {
	var out struct {
		Type   string          `json:"type"`
		Result json.RawMessage `json:"Result"`
	}
	if json.Unmarshal(in.ToolResponse, &out) != nil || out.Type != want {
		return nil
	}
	var raws []json.RawMessage
	if result := bytes.TrimSpace(out.Result); len(result) > 0 && result[0] == '[' {
		if json.Unmarshal(result, &raws) != nil {
			return nil
		}
	} else {
		raws = []json.RawMessage{result}
	}
	var events []spool.Event
	for _, raw := range raws {
		var task taskResult
		if json.Unmarshal(raw, &task) != nil || !session.ValidID(task.TaskID) || task.TaskID == in.SessionID {
			continue
		}
		var status string
		switch {
		case want == "TaskOutput" && task.Status == "completed":
			status = "completed"
		case want == "KillTask" && task.Outcome == "killed":
			status = "aborted"
		default:
			continue
		}
		// A shell task's result has the same shape: only a launch terma saw makes it a subagent.
		if !env.KnownSubagent(subagentDir, in.SessionID, task.TaskID) {
			continue
		}
		attrs := hookrun.AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: name}, task.TaskID, "")
		attrs[semconv.TermaOperationStatusKey] = status
		if status == "completed" {
			if secs, _, ok := hookrun.JSONNumber(task.DurationSecs, false); ok {
				attrs[semconv.TermaOperationDurationMsKey] = int64(math.Round(secs * 1000))
			}
		}
		events = append(events, spool.Event{Name: semconv.TermaSubagentEndEvent, SessionID: in.SessionID, Attrs: attrs})
	}
	return events
}
