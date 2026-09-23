package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- Cursor adapter -----------------------------------------------------------------

// cursorHookInput is the JSON Cursor writes to a hook's stdin. Every event carries the
// conversation id, the model and the workspace roots; sessionStart and sessionEnd add a
// session id of their own. Only the fields terma reads are declared.
type cursorHookInput struct {
	ConversationID      string          `json:"conversation_id"`
	SessionID           string          `json:"session_id"`
	Event               string          `json:"hook_event_name"`
	Model               string          `json:"model"`
	WorkspaceRoots      []string        `json:"workspace_roots"`
	ComposerMode        string          `json:"composer_mode"`
	Reason              string          `json:"reason"`
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
	// SubagentID and ParentConversationID are what subagentStop carries beside, or
	// instead of, the conversation id: the subagent's own conversation and the one that
	// spawned it. A subagent's afterFileEdit can arrive under the first.
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

// id is the session key. The conversation id comes first because it is the one
// identifier present on every event: afterFileEdit has no session_id, and keying on
// anything else would put the edits in a different manifest from the session.
func (in *cursorHookInput) id() string {
	return cmp.Or(in.ConversationID, in.SessionID)
}

// cwd is the workspace Cursor is working in. User-level hooks run from ~/.cursor, so
// the process directory says nothing; the first workspace root is the repository.
func (in *cursorHookInput) cwd(fallback string) string {
	if len(in.WorkspaceRoots) > 0 && in.WorkspaceRoots[0] != "" {
		return in.WorkspaceRoots[0]
	}
	return fallback
}

// cursorModelParams copies the allowlisted model parameters onto an event. Model
// selection is evidence, not proof of the model billed (e.g. Auto).
func cursorModelParams(in *cursorHookInput, a map[string]any) {
	for _, p := range in.ModelParams {
		switch p.ID {
		case "thinking", "context", "effort":
			if len(p.Value) <= 128 {
				a["model_param."+p.ID] = p.Value
			}
		}
	}
}

// readCursorInput refuses a payload without a safe conversation id, so no Cursor handler
// has to check the id again. A subagent hook may name only the conversation that spawned
// it; that is the conversation it is filed under, so it stands in for the missing id.
func readCursorInput(r io.Reader) (*cursorHookInput, error) {
	in, err := readHookInput[cursorHookInput](r)
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

// CursorSessionStart records the conversation as the active session.
func CursorSessionStart(ctx context.Context, env Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		env.logf("not in a git repository: %v", err)
		return nil
	}
	env.announce(r, env.newSession(r, in.id(), cursorTool, in.Model), map[string]any{attrSource: in.ComposerMode})
	env.captureCursorObservation(ctx, r, in, "sessionStart")
	return nil
}

// CursorSessionEnd clears the active session; manifests stay for the commit to come.
func CursorSessionEnd(ctx context.Context, env Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.endSession(r, in.id(), cursorTool, in.Reason)
	env.captureCursorObservation(ctx, r, in, "sessionEnd")
	return nil
}

// CursorFileEdit adds the edited file to the conversation's manifest.
func CursorFileEdit(ctx context.Context, env Env) error {
	in, err := readCursorInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if in.FilePath == "" {
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	extra := map[string]any{}
	boundedAttr(extra, attrTurnID, in.GenerationID)
	env.touch(r, session.Session{ID: in.id(), Tool: cursorTool, Model: in.Model}, "afterFileEdit",
		relativeFiles(r, env.Cwd, []string{in.FilePath}), extra)
	return nil
}
