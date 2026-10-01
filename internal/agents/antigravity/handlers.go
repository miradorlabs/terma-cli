package antigravity

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	antigravityObservationDir = "antigravity-observations"
	antigravityTurnDir        = "antigravity-turns"
)

const sourceAntigravityHook = "antigravity_hook"

// antigravityHookInput is the protojson agy writes to a hook's stdin, the fields terma reads.
type antigravityHookInput struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	ModelName      string   `json:"modelName"`
	// Error is failure text; only its presence is recorded, since the text is content.
	Error string `json:"error"`
	// PostToolUse.
	StepIdx  json.RawMessage `json:"stepIdx"`
	ToolCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
	// PreInvocation and PostInvocation.
	InvocationNum   json.RawMessage `json:"invocationNum"`
	InitialNumSteps json.RawMessage `json:"initialNumSteps"`
	// Stop.
	ExecutionNum      json.RawMessage `json:"executionNum"`
	TerminationReason string          `json:"terminationReason"`
	FullyIdle         *bool           `json:"fullyIdle"`
}

const antigravityTool = "antigravity"

// antigravityConversationEnv carries the payload's conversation id, the fallback when it is omitted.
const antigravityConversationEnv = "ANTIGRAVITY_CONVERSATION_ID"

func (in *antigravityHookInput) id() string {
	return cmp.Or(in.ConversationID, os.Getenv(antigravityConversationEnv))
}

// cwd is the first workspace path, which agy calls the workspace; hooks run in its .agents.
func (in *antigravityHookInput) cwd(fallback string) string {
	if len(in.WorkspacePaths) > 0 && in.WorkspacePaths[0] != "" {
		return in.WorkspacePaths[0]
	}
	return fallback
}

func readAntigravityInput(r io.Reader) (*antigravityHookInput, error) {
	in, err := hookrun.ReadInput[antigravityHookInput](r)
	if err != nil {
		return nil, err
	}
	if !session.ValidID(in.id()) {
		return nil, errors.New("hook input has no safe conversationId")
	}
	return in, nil
}

// antigravityAck writes the empty object every agy hook expects on stdout, even when the
// handler bails out early, so agy never prints a parse warning.
func antigravityAck(env hookrun.Env) {
	if env.Stdout != nil {
		_, _ = io.WriteString(env.Stdout, "{}\n")
	}
}

// preInvocation marks the conversation active and records the turn at each turn's start.
// agy has no SessionStart: invocation 0 of a fresh conversation is where a session begins.
func preInvocation(ctx context.Context, env hookrun.Env) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	invocation, _, invocationKnown := hookrun.JSONNumber(in.InvocationNum, true)
	if invocationKnown && invocation != 0 {
		// A model call in the middle of a turn.
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		env.Logf("not in a git repository: %v", err)
		return nil
	}
	id := in.id()
	sess := env.NewSession(r, id, antigravityTool, in.ModelName)
	now := sess.UpdatedAt
	if active, _ := r.Store.Active(now, 0); active != nil && active.ID == id && !active.StartedAt.IsZero() {
		sess.StartedAt = active.StartedAt
	}
	env.SetActive(r, sess)
	turn := beginAntigravityTurn(env, r, in)
	// A later turn, or a resumed conversation, was announced before.
	if steps, _, stepsKnown := hookrun.JSONNumber(in.InitialNumSteps, true); !stepsKnown || steps <= 1 {
		env.PruneManifests(r, now)
		env.EmitStart(r, sess, nil)
	}
	env.CaptureObservation(ctx, r, hookrun.Observation{
		Tool: antigravityTool, Source: sourceAntigravityHook, StateDir: antigravityObservationDir,
		SessionID: id, Hook: "PreInvocation", TurnID: turn, Attrs: antigravityObservationAttrs(in, "PreInvocation", turn),
	})
	return nil
}

// antigravityEditTools are the tools whose arguments name a changed file; agy exposes
// edit tools per model family, its own taking TargetFile, the others the vendor's key.
var antigravityEditTools = map[string]bool{
	"write_to_file": true, "replace_file_content": true, "multi_replace_file_content": true,
	"create_file": true, "edit_file": true, "delete_file": true,
	"str_replace_editor": true, "notebook_edit": true,
}

var antigravityPathKeys = []string{"TargetFile", "target_file", "file_path", "path", "notebook_path"}

// antigravityEditedPaths pulls the edited file out of a tool call; str_replace_editor's
// `view` reads.
func antigravityEditedPaths(in *antigravityHookInput) []string {
	if in.ToolCall == nil || !antigravityEditTools[in.ToolCall.Name] || len(in.ToolCall.Args) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(in.ToolCall.Args, &args) != nil {
		return nil
	}
	if in.ToolCall.Name == "str_replace_editor" {
		var command string
		if json.Unmarshal(args["command"], &command) == nil && command == "view" {
			return nil
		}
	}
	var out []string
	for _, key := range antigravityPathKeys {
		var p string
		if json.Unmarshal(args[key], &p) == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// postToolUse records every tool step as a `terma.tool.call`, since agy has no other
// export, and the file an edit changed; `toolCall.args` is opened only for that path.
func postToolUse(ctx context.Context, env hookrun.Env) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	id := in.id()
	turn := antigravityTurnID(r, id)
	call, called := antigravityToolCallAttrs(in, turn)
	if called {
		call[hookrun.AttrVersion] = env.Version
		env.EmitFor(r, spool.Event{Name: hookrun.EventToolCall, SessionID: id, Repo: r.Name, Attrs: call})
	}

	// No toolCall yields no paths, so this guard also makes in.ToolCall safe below.
	files := hookrun.RelativeFiles(r, env.Cwd, antigravityEditedPaths(in))
	if len(files) == 0 {
		return nil
	}
	// The step's terma.tool.call ids: this is what that call changed, not a second call.
	ids := map[string]any{}
	for _, k := range []string{hookrun.AttrToolCallID, "step_idx", hookrun.AttrTurnID} {
		if v, ok := call[k]; ok {
			ids[k] = v
		}
	}
	env.Touch(r, session.Session{ID: id, Tool: antigravityTool, Model: in.ModelName}, in.ToolCall.Name, files, ids)
	return nil
}

// antigravityToolCallAttrs is the body of a step's terma.tool.call. agy issues no call id;
// its step index only grows within a conversation, so it becomes `tool_call_id`. `status`
// is agy's: a shell command that exits non-zero is a completed step.
func antigravityToolCallAttrs(in *antigravityHookInput, turn string) (map[string]any, bool) {
	a := hookrun.EvidenceAttrs(antigravityTool, sourceAntigravityHook, "PostToolUse")
	if in.ToolCall != nil && hookrun.ShortLabel(in.ToolCall.Name) {
		a[hookrun.AttrToolName] = in.ToolCall.Name
	}
	if step, _, ok := hookrun.JSONNumber(in.StepIdx, true); ok {
		a["step_idx"] = int64(step)
		a[hookrun.AttrToolCallID] = "step-" + strconv.FormatUint(uint64(step), 10)
	}
	if _, named := a[hookrun.AttrToolName]; !named {
		if _, identified := a[hookrun.AttrToolCallID]; !identified {
			return nil, false
		}
	}
	if turn != "" {
		a[hookrun.AttrTurnID] = turn
	}
	hookrun.BoundedAttr(a, hookrun.AttrModel, in.ModelName)
	a[hookrun.AttrStatus] = "completed"
	if in.Error != "" {
		a[hookrun.AttrStatus] = "error"
	}
	return a, true
}

// postInvocation and stop record the turn's shape as observations: evidence of activity,
// never usage, since agy reports no token counts.
func postInvocation(ctx context.Context, env hookrun.Env) error {
	return antigravityObserve(ctx, env, "PostInvocation")
}

func stop(ctx context.Context, env hookrun.Env) error {
	return antigravityObserve(ctx, env, "Stop")
}

func antigravityObserve(ctx context.Context, env hookrun.Env, hook string) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if hook == "Stop" {
		// Keep the session fresh for the commit that may follow.
		now := env.Time()
		if active, _ := r.Store.Active(now, 0); active != nil && active.ID == in.id() {
			active.UpdatedAt, active.Model = now, cmp.Or(in.ModelName, active.Model)
			_ = r.Store.SetActive(*active)
		}
	}
	turn := antigravityTurnID(r, in.id())
	env.CaptureObservation(ctx, r, hookrun.Observation{
		Tool: antigravityTool, Source: sourceAntigravityHook, StateDir: antigravityObservationDir,
		SessionID: in.id(), Hook: hook, TurnID: turn, Attrs: antigravityObservationAttrs(in, hook, turn),
	})
	return nil
}

func antigravityObservationAttrs(in *antigravityHookInput, hook, turn string) map[string]any {
	a := hookrun.EvidenceAttrs(antigravityTool, sourceAntigravityHook, hook)
	for _, k := range []string{"usage_status", "funding_status", "quota_status", "account_status"} {
		a[k] = hookrun.StatusUnavailable
	}
	hookrun.BoundedAttr(a, hookrun.AttrModel, in.ModelName)
	if turn != "" {
		a[hookrun.AttrTurnID] = turn
	}
	for k, v := range map[string]json.RawMessage{
		"invocation_num": in.InvocationNum, "initial_num_steps": in.InitialNumSteps, "execution_num": in.ExecutionNum,
	} {
		if value, _, ok := hookrun.JSONNumber(v, true); ok {
			a[k] = int64(value)
		}
	}
	if hook == "Stop" {
		if in.TerminationReason != "" && len(in.TerminationReason) <= 128 {
			a["termination_reason"] = in.TerminationReason
		}
		if in.FullyIdle != nil {
			a["fully_idle"] = *in.FullyIdle
		}
		// Only an error's presence travels, never its text.
		a[hookrun.AttrStatus] = "ok"
		if in.Error != "" {
			a[hookrun.AttrStatus] = "error"
		}
	}
	return a
}
