package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// claudeHookInput is the subset of a hook's stdin JSON that terma reads.
type claudeHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	Source    string `json:"source"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
	Model     string `json:"model"`
	PromptID  string `json:"prompt_id"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	ToolInput struct {
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
	env.Announce(r, env.NewSession(r, in.SessionID, claudeTool, in.Model), map[string]any{hookrun.AttrSource: in.Source})
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
	env.EndSession(r, in.SessionID, claudeTool, in.Reason)
	return nil
}

// stop refreshes account evidence before the dispatcher starts delivery.
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
	return nil
}

// claudeShellTool is the tool whose edits only the working tree shows.
const claudeShellTool = "Bash"

// preToolUse snapshots the working tree before a Bash call, for postToolUse to diff. It
// prints nothing: a PreToolUse reply can deny the call.
func preToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil || in.ToolName != claudeShellTool || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.ShellBefore(ctx, r, in.SessionID, in.ToolUseID)
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
	sess := session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}
	if in.ToolName == claudeShellTool {
		env.ShellAfter(ctx, r, sess, in.ToolName, in.ToolUseID, hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType))
		return nil
	}
	paths := append([]string{in.ToolInput.FilePath, in.ToolInput.NotebookPath}, editPaths(in)...)
	// Inside a subagent the manifest stays the session's; the event names the agent.
	env.Touch(r, sess, in.ToolName,
		hookrun.RelativeFiles(r, env.Cwd, paths), hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType))
	return nil
}

func editPaths(in *claudeHookInput) []string {
	var out []string
	for _, e := range in.ToolInput.Edits {
		out = append(out, e.FilePath)
	}
	return out
}
