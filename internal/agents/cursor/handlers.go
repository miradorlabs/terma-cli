package cursor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

var cursorObservationDir = hookrun.AgentStateDir(Agent{}.Name(), "observations")

// sourceCursorHook is terma.evidence.source for what a Cursor hook reported.
const sourceCursorHook = "cursor_hook"

// cursorHookInput is what Cursor writes to a hook's stdin, the fields terma reads.
type cursorHookInput struct {
	ConversationID      string          `json:"conversation_id"`
	SessionID           string          `json:"session_id"`
	Event               string          `json:"hook_event_name"`
	Model               string          `json:"model"`
	WorkspaceRoots      []string        `json:"workspace_roots"`
	FilePath            string          `json:"file_path"`
	GenerationID        string          `json:"generation_id"`
	ModelID             string          `json:"model_id"`
	CursorVersion       string          `json:"cursor_version"`
	UserEmail           string          `json:"user_email"`
	Status              string          `json:"status"`
	LoopCount           json.RawMessage `json:"loop_count"`
	InputTokens         json.RawMessage `json:"input_tokens"`
	OutputTokens        json.RawMessage `json:"output_tokens"`
	CacheReadTokens     json.RawMessage `json:"cache_read_tokens"`
	CacheWriteTokens    json.RawMessage `json:"cache_write_tokens"`
	ContextTokens       json.RawMessage `json:"context_tokens"`
	ContextWindowSize   json.RawMessage `json:"context_window_size"`
	ContextUsagePercent json.RawMessage `json:"context_usage_percent"`
	Trigger             string          `json:"trigger"`
	SubagentType        string          `json:"subagent_type"`
	// subagentStop's own and spawning conversations; a subagent's afterFileEdit can
	// arrive under SubagentID.
	SubagentID           string          `json:"subagent_id"`
	ParentConversationID string          `json:"parent_conversation_id"`
	DurationMs           json.RawMessage `json:"duration_ms"`
	MessageCount         json.RawMessage `json:"message_count"`
	ToolCallCount        json.RawMessage `json:"tool_call_count"`
	ModifiedFiles        []string        `json:"modified_files"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	Duration             json.RawMessage `json:"duration"`
	FailureType          string          `json:"failure_type"`
	IsInterrupt          json.RawMessage `json:"is_interrupt"`
	ModelParams          []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"model_params"`
}

const cursorTool = "cursor"

// id is the session key: the conversation id first, since afterFileEdit has no session_id.
func (in *cursorHookInput) id() string {
	return cmp.Or(in.ConversationID, in.SessionID)
}

// cwd is the first workspace root: user-level hooks run from ~/.cursor.
func (in *cursorHookInput) cwd(fallback string) string {
	if len(in.WorkspaceRoots) > 0 && in.WorkspaceRoots[0] != "" {
		return in.WorkspaceRoots[0]
	}
	return fallback
}

// cursorModelParams are the allowlisted model parameters and the keys they leave under.
var cursorModelParams = map[string]string{
	"thinking": semconv.TermaModelParamThinkingKey,
	"context":  semconv.TermaModelParamContextKey,
	"effort":   semconv.GenAIRequestReasoningLevelKey,
}

// copyModelParams copies the allowlisted model parameters onto an event; a selection is
// evidence, not proof of the model billed (Auto).
func copyModelParams(in *cursorHookInput, a map[string]any) {
	for _, p := range in.ModelParams {
		if key, ok := cursorModelParams[p.ID]; ok && len(p.Value) <= 128 {
			a[key] = p.Value
		}
	}
}

// readCursorInput refuses a payload without a safe conversation id; a subagent hook's
// spawning conversation stands in for a missing one.
func readCursorInput(r io.Reader) (*cursorHookInput, error) {
	in, err := hookrun.ReadInput[cursorHookInput](r)
	if err != nil {
		return nil, err
	}
	if !session.ValidID(in.id()) {
		if !session.ValidID(in.ParentConversationID) {
			return nil, errors.New("hook input has no safe conversation_id")
		}
		in.ConversationID, in.SessionID = in.ParentConversationID, ""
	}
	return in, nil
}

func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		env.Logf("not in a git repository: %v", err)
		return nil
	}
	env.Announce(r, env.NewSession(r, in.id(), cursorTool, in.Model), nil)
	captureCursorObservation(ctx, env, r, in, "sessionStart")
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.EndSession(r, in.id(), cursorTool)
	captureCursorObservation(ctx, env, r, in, "sessionEnd")
	return nil
}

func fileEdit(ctx context.Context, env hookrun.Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if in.FilePath == "" {
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	extra := map[string]any{}
	hookrun.BoundedAttr(extra, semconv.TermaTurnIDKey, in.GenerationID)
	env.Touch(r, session.Session{ID: in.id(), Tool: cursorTool, Model: in.Model}, "afterFileEdit", []string{in.FilePath}, extra)
	return nil
}
