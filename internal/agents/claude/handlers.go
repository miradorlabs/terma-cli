package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// claudeHookInput is the JSON Claude Code writes to a hook's stdin. Only the fields
// terma reads are declared; everything else is ignored.
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
		// SubagentType is the Agent tool's: which kind of subagent was asked for. The
		// task's description and prompt sit beside it and are not decoded.
		SubagentType string `json:"subagent_type"`
	} `json:"tool_input"`
	// ToolResponse is kept undecoded. Only the Agent tool's is ever opened, and then
	// into claudeAgentResult, which names the fields it wants and no others: for an
	// edit this holds the file's content, which is nothing to do with terma.
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

// sessionStart records the announced session as active and spools the start.
func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		env.Logf("not in a git repository: %v", err)
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, claudeTool, in.Model), map[string]any{hookrun.AttrSource: in.Source})
	pruneClaudeSubagents(env)
	captureClaudeAccount(env, r, in)
	return nil
}

// sessionEnd clears the active session (manifests stay: the work may still be
// uncommitted) and spools the end.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
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

// postToolUse adds the edited files to the session's manifest.
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
	// Inside a subagent the payload keeps the parent's session_id and names the agent:
	// the manifest stays the session's, the event says which agent did the editing.
	env.Touch(r, session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}, in.ToolName,
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
