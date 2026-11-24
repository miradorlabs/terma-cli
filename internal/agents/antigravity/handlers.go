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
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// sourceAntigravityHook is terma.evidence.source for what an agy hook reported.
const sourceAntigravityHook = "antigravity_hook"

var (
	antigravityObservationDir = hookrun.AgentStateDir(Agent{}.Name(), "observations")
	antigravityTurnDir        = hookrun.AgentStateDir(Agent{}.Name(), "turns")
)

// antigravityHookInput is the protojson agy writes to a hook's stdin, the fields terma reads.
type antigravityHookInput struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	Cwd            string   `json:"cwd"`
	ModelName      string   `json:"modelName"`
	// TranscriptPath is the conversation's transcript_full.jsonl (agy 1.5+).
	TranscriptPath string `json:"transcriptPath"`
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

// cwd is the agent's working directory (agy 1.5.4+), else the first workspace path; a
// hook itself runs in the folder holding hooks.json.
func (in *antigravityHookInput) cwd(fallback string) string {
	if in.Cwd != "" {
		return in.Cwd
	}
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

// antigravityInput reads a hook's payload. One it cannot read ends the turn terma was
// following in the conversation agy's environment names: agy repeats the prompt in every
// payload, so a prompt past hookrun.MaxInput hides its whole turn's hooks, the first
// included, and no later hook may send that turn's calls as the turn before's.
func antigravityInput(env hookrun.Env) (*antigravityHookInput, bool) {
	in, err := readAntigravityInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		endAntigravityTurnInEnvironment(env)
		return nil, false
	}
	return in, true
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
	in, ok := antigravityInput(env)
	if !ok {
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		// At any invocation, not only the turn's first.
		env.Logf("not in a git repository: %v", err)
		antigravitySkipCalls(env, in)
		return nil
	}
	invocation, _, invocationKnown := hookrun.JSONNumber(in.InvocationNum, true)
	if invocationKnown && invocation != 0 {
		// A model call in the middle of a turn.
		return nil
	}
	// The turn is recorded first: agy calls the model only once this hook returns.
	turn := beginAntigravityTurn(env, r, in)
	id := in.id()
	// One hold of the conversation: this hook's session start, then the transcript read (the
	// turn's prompt, and a cancelled turn's last call passed, see turn.go), each stamped.
	conv := openAntigravityConversation(ctx, env, r, in)
	defer conv.close()
	version := lazyAntigravityVersion(ctx, env)
	sess := env.NewSession(r, id, antigravityTool, in.ModelName)
	now := sess.UpdatedAt
	if active, _ := r.Store.Active(now, 0); active != nil && active.ID == id && !active.StartedAt.IsZero() {
		sess.StartedAt = active.StartedAt
	}
	env.SetActive(r, sess)
	// A later turn, or a resumed conversation, was announced before.
	if steps, _, stepsKnown := hookrun.JSONNumber(in.InitialNumSteps, true); !stepsKnown || steps <= 1 {
		env.PruneManifests(r, now)
		conv.at(env).EmitStart(r, sess, withAgentVersion(map[string]any{}, version))
	}
	// After the session start, so on a conversation's first turn the start sorts before its
	// prompt.
	read := conv.read(ctx)
	conv.at(env).CaptureObservation(ctx, r, hookrun.Observation{
		Tool: antigravityTool, Source: sourceAntigravityHook, StateDir: antigravityObservationDir,
		SessionID: id, Hook: "PreInvocation", TurnID: turn, Attrs: antigravityObservationAttrs(in, "PreInvocation", turn, read, version),
	})
	return nil
}

// antigravityEditTools are the tools whose arguments name a changed file; agy exposes
// edit tools per model family and release: through 1.2 its own took TargetFile
// (write_to_file, replace_file_content), from 1.5 they take Path (write_file, edit_file),
// and the others the vendor's key.
var antigravityEditTools = map[string]bool{
	"write_to_file": true, "replace_file_content": true, "multi_replace_file_content": true,
	"write_file": true, "create_file": true, "edit_file": true, "delete_file": true,
	"str_replace_editor": true, "notebook_edit": true,
}

var antigravityPathKeys = []string{"TargetFile", "Path", "target_file", "file_path", "path", "notebook_path"}

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
// export, with its arguments where the content rules allow, the file an edit changed and the
// skill a read loaded. Under one hold of the conversation it reads the transcript, then spools
// the call, so the model call that asked for the tool sorts before it. Whether the call's
// content is refused is judged from its own arguments, with nothing stored: the reader judges
// the call line's copy of them by the same rule (transcript_read.go).
func postToolUse(ctx context.Context, env hookrun.Env) error {
	defer antigravityAck(env)
	in, ok := antigravityInput(env)
	if !ok {
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		antigravitySkipCalls(env, in)
		return nil
	}
	id := in.id()
	conv := openAntigravityConversation(ctx, env, r, in)
	defer conv.close()
	refused := in.ToolCall != nil && conv.refuses(in.ToolCall.Name, in.ToolCall.Args)
	// The turn as it stands once this hook holds the conversation.
	turn := antigravityTurnID(env, r, id)
	call, called := antigravityToolCallAttrs(in, turn, lazyAntigravityVersion(ctx, env))
	if step, stepped := call[semconv.TermaStepIndexKey].(int64); stepped {
		conv.call.step = step
	}
	conv.read(ctx)
	// A skill's use rides on the read that loaded it; its definition goes once per conversation.
	// Both are content of both kinds: they go only where the call's arguments and a prompt may.
	var skill hookrun.Skill
	used := false
	if content := conv.contentFor(); !refused && content.prompts && content.tools {
		skill, used = antigravityUsedSkill(env, r, in)
	}
	if called {
		if used {
			nameSkill(call, skill)
		}
		// Arguments go only in this hook's own turn, under the lock, unless refused.
		//
		// Accepted risk: arguments and a result are queued under the hook's checkout, though the
		// file they name may be in another collected checkout; if the policy drops that other
		// checkout before the queue is flushed, delivery, which judges the hook's checkout,
		// still sends them. Upgrade path: queue the file's repository with the event and recheck
		// it at delivery.
		if turn != "" && conv.held && !refused && in.ToolCall != nil && conv.contentFor().tools {
			if args, ok := cutArguments(in.ToolCall.Args); ok {
				call[semconv.GenAIToolCallArgumentsKey] = args
			}
		}
		env.EmitFor(r, spool.Event{Name: semconv.TermaToolCallEvent, SessionID: id, Time: conv.callTime(), Attrs: call})
	}
	if used {
		hookrun.SpoolSkills(conv.at(env), r, antigravitySkillStateDir, id, antigravitySkillAttrs(), []hookrun.Skill{skill})
	}

	// A failed edit changed nothing. No toolCall yields no paths, so this guard also makes
	// in.ToolCall safe below.
	files := antigravityEditedPaths(in)
	if len(files) == 0 || in.Error != "" {
		return nil
	}
	env = conv.at(env)
	// The step's terma.tool.call ids: this is what that call changed, not a second call.
	ids := map[string]any{}
	for _, k := range []string{semconv.GenAIToolCallIDKey, semconv.TermaStepIndexKey, semconv.TermaTurnIDKey} {
		if v, ok := call[k]; ok {
			ids[k] = v
		}
	}
	env.Touch(r, session.Session{ID: id, Tool: antigravityTool, Model: in.ModelName}, in.ToolCall.Name, files, ids)
	return nil
}

// antigravityToolCallAttrs is the body of a step's terma.tool.call. agy issues no call id;
// its step index only grows within a conversation, so it becomes the gen_ai.tool.call.id.
// terma.operation.status is agy's: a shell command that exits non-zero is a completed step.
// version is asked only for a call that goes.
func antigravityToolCallAttrs(in *antigravityHookInput, turn string, version func() string) (map[string]any, bool) {
	a := hookrun.EvidenceAttrs(antigravityTool, sourceAntigravityHook, "PostToolUse")
	if in.ToolCall != nil && hookrun.ShortLabel(in.ToolCall.Name) {
		a[semconv.GenAIToolNameKey] = in.ToolCall.Name
	}
	if step, _, ok := hookrun.JSONNumber(in.StepIdx, true); ok {
		a[semconv.TermaStepIndexKey] = int64(step)
		a[semconv.GenAIToolCallIDKey] = "step-" + strconv.FormatUint(uint64(step), 10)
	}
	if _, named := a[semconv.GenAIToolNameKey]; !named {
		if _, identified := a[semconv.GenAIToolCallIDKey]; !identified {
			return nil, false
		}
	}
	if turn != "" {
		a[semconv.TermaTurnIDKey] = turn
	}
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, in.ModelName)
	a[semconv.TermaOperationStatusKey] = "completed"
	if in.Error != "" {
		a[semconv.TermaOperationStatusKey] = "error"
	}
	return withAgentVersion(a, version), true
}

// postInvocation and stop record the turn's shape as observations, and send the calls
// the transcript recorded since the last hook, each as a terma.model_call.
func postInvocation(ctx context.Context, env hookrun.Env) error {
	return antigravityObserve(ctx, env, "PostInvocation")
}

func stop(ctx context.Context, env hookrun.Env) error {
	return antigravityObserve(ctx, env, "Stop")
}

func antigravityObserve(ctx context.Context, env hookrun.Env, hook string) error {
	defer antigravityAck(env)
	in, ok := antigravityInput(env)
	if !ok {
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		antigravitySkipCalls(env, in)
		return nil
	}
	if hook == "PostInvocation" {
		// Invocation 0 carries the turn's first step, as its PreInvocation did, so it can
		// tell a stale record (see turn.go).
		invocation, _, invocationKnown := hookrun.JSONNumber(in.InvocationNum, true)
		if steps, _, stepsKnown := hookrun.JSONNumber(in.InitialNumSteps, true); invocationKnown && invocation == 0 && stepsKnown {
			endAntigravityTurnUnlessBegunAt(env, in.id(), int64(steps))
		}
	}
	if hook == "Stop" {
		// Keep the session fresh for the commit that may follow.
		now := env.Time()
		if active, _ := r.Store.Active(now, 0); active != nil && active.ID == in.id() {
			active.UpdatedAt, active.Model = now, cmp.Or(in.ModelName, active.Model)
			_ = r.Store.SetActive(*active)
		}
	}
	begun := antigravityTurnBegun(env, in.id())
	turn := antigravityTurnID(env, r, in.id())
	conv := openAntigravityConversation(ctx, env, r, in)
	defer conv.close()
	read := conv.read(ctx)
	if hook == "Stop" {
		// Stop ends the turn it found and no other; a prompt or a wake after it starts with
		// a PreInvocation at invocation 0, which records the turn again.
		endAntigravityTurnBegun(env, in.id(), begun)
	}
	conv.at(env).CaptureObservation(ctx, r, hookrun.Observation{
		Tool: antigravityTool, Source: sourceAntigravityHook, StateDir: antigravityObservationDir,
		SessionID: in.id(), Hook: hook, TurnID: turn, Attrs: antigravityObservationAttrs(in, hook, turn, read, lazyAntigravityVersion(ctx, env)),
	})
	return nil
}

// antigravityObservationAttrs is a hook's observation. A hook carries no plan, quota or
// account (the status line does), and no token counts: when the hook read the transcript,
// the calls carry them as terma.model_call, so the observation makes no claim about usage;
// when it could not, its usage is unavailable. version is the running binary's (version.go).
func antigravityObservationAttrs(in *antigravityHookInput, hook, turn string, callsRead bool, version func() string) map[string]any {
	a := hookrun.EvidenceAttrs(antigravityTool, sourceAntigravityHook, hook)
	for _, k := range []string{semconv.TermaFundingStatusKey, semconv.TermaQuotaStatusKey, semconv.TermaAccountStatusKey} {
		a[k] = hookrun.StatusUnavailable
	}
	if !callsRead {
		a[semconv.TermaUsageStatusKey] = hookrun.StatusUnavailable
	}
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, in.ModelName)
	if turn != "" {
		a[semconv.TermaTurnIDKey] = turn
	}
	for k, v := range map[string]json.RawMessage{
		semconv.TermaInvocationNumberKey: in.InvocationNum, semconv.TermaInvocationInitialStepsKey: in.InitialNumSteps, semconv.TermaExecutionNumberKey: in.ExecutionNum,
	} {
		if value, _, ok := hookrun.JSONNumber(v, true); ok {
			a[k] = int64(value)
		}
	}
	if hook == "Stop" {
		if in.TerminationReason != "" && len(in.TerminationReason) <= 128 {
			a[semconv.TermaTerminationReasonKey] = in.TerminationReason
		}
		if in.FullyIdle != nil {
			a[semconv.TermaAgentFullyIdleKey] = *in.FullyIdle
		}
		// Only an error's presence travels, never its text.
		a[semconv.TermaOperationStatusKey] = "ok"
		if in.Error != "" {
			a[semconv.TermaOperationStatusKey] = "error"
		}
	}
	return withAgentVersion(a, version)
}

// antigravitySkipCalls is a hook that could not resolve where it ran, or ran where the
// team's policy collects nothing: it ends the turn terma was following and reads nothing
// (see turn.go).
func antigravitySkipCalls(env hookrun.Env, in *antigravityHookInput) {
	endAntigravityTurn(env, in.id())
}
