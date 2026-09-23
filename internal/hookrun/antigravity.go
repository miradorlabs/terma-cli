package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// --- Antigravity adapter ---------------------------------------------------------------

// antigravityHookInput is the JSON Antigravity CLI writes to a hook's stdin (protojson,
// so every key is camelCase). Every event carries the conversation id, the model, the
// workspace roots and the paths of agy's own transcript and artifact directory; the
// per-event fields are declared where terma reads them. Verified against agy 1.2.4.
type antigravityHookInput struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	ModelName      string   `json:"modelName"`
	// Error is "" on success and the tool's or the loop's failure text otherwise. Only
	// its presence is recorded: the text is content.
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

// antigravityConversationEnv is set in every hook's environment by agy, and is the same
// id the payload carries. It is the fallback for a payload that omits it.
const antigravityConversationEnv = "ANTIGRAVITY_CONVERSATION_ID"

// id is the session key: agy's conversation, which every event carries.
func (in *antigravityHookInput) id() string {
	return cmp.Or(in.ConversationID, os.Getenv(antigravityConversationEnv))
}

// cwd is the workspace agy is working in. Hooks run from the directory that holds
// hooks.json (`<workspace>/.agents`), so the process directory would do, but the first
// workspace path is what agy itself calls the workspace and is the repository.
func (in *antigravityHookInput) cwd(fallback string) string {
	if len(in.WorkspacePaths) > 0 && in.WorkspacePaths[0] != "" {
		return in.WorkspacePaths[0]
	}
	return fallback
}

func readAntigravityInput(r io.Reader) (*antigravityHookInput, error) {
	in, err := readHookInput[antigravityHookInput](r)
	if err != nil {
		return nil, err
	}
	if !session.ValidID(in.id()) {
		return nil, errors.New("hook input has no safe conversationId")
	}
	return in, nil
}

// antigravityAck is what every agy hook expects on stdout: an empty object means "no
// decision, no injected steps, carry on". Written even when the handler bails out early,
// because agy documents the object as the contract and terma never wants to be the
// hook that made an agent print a parse warning.
func antigravityAck(env Env) {
	if env.Stdout != nil {
		_, _ = io.WriteString(env.Stdout, "{}\n")
	}
}

// AntigravityPreInvocation records the conversation as the active session at the start
// of each turn, and the turn itself (see antigravity_turn.go). agy has no SessionStart:
// the first invocation of a fresh conversation (invocation 0 with only the user's message
// on the transcript) is where a session begins, and later turns refresh the record so the
// TTL fallback tracks real activity. Every turn's start is observed, so a reader has the
// moment the person's message arrived and not only the model calls that followed.
func AntigravityPreInvocation(ctx context.Context, env Env) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	invocation, _, invocationKnown := cursorNumber(in.InvocationNum, true)
	if invocationKnown && invocation != 0 {
		// A model call in the middle of a turn: nothing about the session changes.
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		env.logf("not in a git repository: %v", err)
		return nil
	}
	id := in.id()
	sess := env.newSession(r, id, antigravityTool, in.ModelName)
	now := sess.UpdatedAt
	if active, _ := r.store.Active(now, 0); active != nil && active.ID == id && !active.StartedAt.IsZero() {
		sess.StartedAt = active.StartedAt
	}
	env.setActive(r, sess)
	turn := env.beginAntigravityTurn(r, in)
	// A later turn, or a resumed conversation, was announced before.
	if steps, _, stepsKnown := cursorNumber(in.InitialNumSteps, true); !stepsKnown || steps <= 1 {
		env.pruneManifests(r, now)
		env.emitStart(r, sess, nil)
	}
	env.captureObservation(ctx, r, observation{
		tool: antigravityTool, source: sourceAntigravityHook, stateDir: antigravityObservationDir,
		sessionID: id, hook: "PreInvocation", turnID: turn, attrs: antigravityObservationAttrs(in, "PreInvocation", turn),
	})
	return nil
}

// antigravityEditTools are the tool names whose arguments name a file the agent
// changed. agy exposes different edit tools per model family; the path argument is
// TargetFile for its own tools and follows the model vendor's convention otherwise.
var antigravityEditTools = map[string]bool{
	"write_to_file": true, "replace_file_content": true, "multi_replace_file_content": true,
	"create_file": true, "edit_file": true, "delete_file": true,
	"str_replace_editor": true, "notebook_edit": true,
}

// antigravityPathKeys are the argument names that carry the edited file's path.
var antigravityPathKeys = []string{"TargetFile", "target_file", "file_path", "path", "notebook_path"}

// antigravityEditedPaths pulls the edited file out of a tool call, or nothing for a
// tool that reads. str_replace_editor's `view` command reads too.
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

// AntigravityPostToolUse records one finished tool step, and the file it edited when it
// edited one. Every tool step arrives here (the hook is unmatched), so every step is a
// `terma.tool.call`: agy has no other export, and a session that only read, searched and
// ran commands otherwise looks like one that did nothing. `toolCall.args` is opened for
// one thing, an edit tool's path; a command line, a query or file content is never read.
func AntigravityPostToolUse(ctx context.Context, env Env) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	id := in.id()
	turn := antigravityTurnID(r, id)
	call, called := antigravityToolCallAttrs(in, turn)
	if called {
		call[attrVersion] = env.Version
		env.emitFor(r, spool.Event{Name: EventToolCall, SessionID: id, Repo: repoName(r.root), Attrs: call})
	}

	// A payload without a toolCall yields no paths, so this guard is also what makes
	// in.ToolCall safe to read below.
	files := relativeFiles(r, env.Cwd, antigravityEditedPaths(in))
	if len(files) == 0 {
		return nil
	}
	// The same ids as the step's terma.tool.call: this event is what that call changed,
	// not a second call, and the pair is how a reader tells.
	ids := map[string]any{}
	for _, k := range []string{attrToolCallID, "step_idx", attrTurnID} {
		if v, ok := call[k]; ok {
			ids[k] = v
		}
	}
	env.touch(r, session.Session{ID: id, Tool: antigravityTool, Model: in.ModelName}, in.ToolCall.Name, files, ids)
	return nil
}

// antigravityToolCallAttrs is the body of a step's terma.tool.call: the tool's name, the
// step's identity, the turn and how it ended. agy issues no call id; the step index is
// its own, only grows within a conversation — across turns and resumes alike — and is
// what makes a redelivered event the same event, so it becomes `tool_call_id`. A payload
// naming neither a tool nor a step is dropped.
//
// `status` is agy's own reading: `error` is set when the tool failed, not when what it ran
// did — a shell command that exits non-zero is a completed step. agy reports no duration,
// so none is sent.
func antigravityToolCallAttrs(in *antigravityHookInput, turn string) (map[string]any, bool) {
	a := evidenceAttrs(antigravityTool, sourceAntigravityHook, "PostToolUse")
	if in.ToolCall != nil && shortLabel(in.ToolCall.Name) {
		a[attrToolName] = in.ToolCall.Name
	}
	if step, _, ok := cursorNumber(in.StepIdx, true); ok {
		a["step_idx"] = int64(step)
		a[attrToolCallID] = "step-" + strconv.FormatUint(uint64(step), 10)
	}
	if _, named := a[attrToolName]; !named {
		if _, identified := a[attrToolCallID]; !identified {
			return nil, false
		}
	}
	if turn != "" {
		a[attrTurnID] = turn
	}
	boundedAttr(a, attrModel, in.ModelName)
	a[attrStatus] = "completed"
	if in.Error != "" {
		a[attrStatus] = "error"
	}
	return a, true
}

// AntigravityPostInvocation and AntigravityStop record the turn's shape as observations:
// which model answered, how many model calls the turn took, how it ended. They are
// evidence of activity, never usage — agy's hooks carry no token counts, and its
// transcripts carry none either. Do not derive spend or quota from them.
func AntigravityPostInvocation(ctx context.Context, env Env) error {
	return antigravityObserve(ctx, env, "PostInvocation")
}

// AntigravityStop fires when the execution loop ends: the turn is over and the person
// is reading. The dispatcher starts a detached spool flush afterwards.
func AntigravityStop(ctx context.Context, env Env) error {
	return antigravityObserve(ctx, env, "Stop")
}

func antigravityObserve(ctx context.Context, env Env, hook string) error {
	defer antigravityAck(env)
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	if hook == "Stop" {
		// The turn is done: keep the session fresh for the commit that may follow.
		now := env.now()
		if active, _ := r.store.Active(now, 0); active != nil && active.ID == in.id() {
			active.UpdatedAt, active.Model = now, cmp.Or(in.ModelName, active.Model)
			_ = r.store.SetActive(*active)
		}
	}
	turn := antigravityTurnID(r, in.id())
	env.captureObservation(ctx, r, observation{
		tool: antigravityTool, source: sourceAntigravityHook, stateDir: antigravityObservationDir,
		sessionID: in.id(), hook: hook, turnID: turn, attrs: antigravityObservationAttrs(in, hook, turn),
	})
	return nil
}

func antigravityObservationAttrs(in *antigravityHookInput, hook, turn string) map[string]any {
	a := evidenceAttrs(antigravityTool, sourceAntigravityHook, hook)
	for _, k := range []string{"usage_status", "funding_status", "quota_status", "account_status"} {
		a[k] = statusUnavailable
	}
	boundedAttr(a, attrModel, in.ModelName)
	if turn != "" {
		a[attrTurnID] = turn
	}
	for k, v := range map[string]json.RawMessage{
		"invocation_num": in.InvocationNum, "initial_num_steps": in.InitialNumSteps, "execution_num": in.ExecutionNum,
	} {
		if value, _, ok := cursorNumber(v, true); ok {
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
		// An error is not automatically a billing or limit error; only its presence
		// travels, never its text.
		a[attrStatus] = "ok"
		if in.Error != "" {
			a[attrStatus] = "error"
		}
	}
	return a
}
