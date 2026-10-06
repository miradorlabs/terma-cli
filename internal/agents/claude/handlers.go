package claude

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

// claudeHookInput is the subset of a hook's stdin JSON that terma reads.
type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	Error          string `json:"error"`
	Source         string `json:"source"`
	Model          string `json:"model"`
	PromptID       string `json:"prompt_id"`
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	ToolName       string `json:"tool_name"`
	ToolUseID      string `json:"tool_use_id"`
	ToolInput      struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Edits        []struct {
			FilePath string `json:"file_path"`
		} `json:"edits"`
		// SubagentType is the Agent tool's; the task's description and prompt beside it are not decoded.
		SubagentType string `json:"subagent_type"`
	} `json:"tool_input"`
	// ToolResponse is opened only for the Agent tool, into claudeAgentResult: for an edit it holds
	// file content.
	ToolResponse json.RawMessage `json:"tool_response"`
}

func readClaudeInput(r io.Reader) (*claudeHookInput, error) {
	in, err := hookrun.ReadInput[claudeHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

const claudeTool = "claude-code"

func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, claudeTool, in.Model), nil)
	pruneClaudeSubagents(env)
	captureClaudeAccount(env, r, in)
	return nil
}

// sessionEnd clears the active session and spools the end; manifests stay for uncommitted work.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	limitFromTranscript(env, r, in)
	env.EndSession(r, in.SessionID, claudeTool)
	// A /rename after the last turn ends no turn.
	captureClaudeTitle(env, r, in)
	return nil
}

// stop refreshes account evidence and the session's title before the dispatcher starts delivery.
func stop(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	captureClaudeAccount(env, r, in)
	captureClaudeTitle(env, r, in)
	return nil
}

func postToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if isClaudeAgentTool(in.ToolName) {
		claudeSubagentCall(env, r, in)
		return nil
	}
	paths := append([]string{in.ToolInput.FilePath, in.ToolInput.NotebookPath}, editPaths(in)...)
	// Inside a subagent the manifest stays the session's; the event names the agent.
	attrs := hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType)
	hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolUseID)
	env.Touch(r, session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}, in.ToolName, paths, attrs)
	return nil
}

func editPaths(in *claudeHookInput) []string {
	var out []string
	for _, e := range in.ToolInput.Edits {
		out = append(out, e.FilePath)
	}
	return out
}
