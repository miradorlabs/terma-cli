package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/shellwrites"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// claudeHookInput is the subset of a hook's stdin JSON that terma reads.
type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	// AgentTranscriptPath is a subagent's own transcript, on SubagentStop.
	AgentTranscriptPath string `json:"agent_transcript_path"`
	Cwd                 string `json:"cwd"`
	Event               string `json:"hook_event_name"`
	Error               string `json:"error"`
	Source              string `json:"source"`
	Model               string `json:"model"`
	PromptID            string `json:"prompt_id"`
	AgentID             string `json:"agent_id"`
	AgentType           string `json:"agent_type"`
	ToolName            string `json:"tool_name"`
	ToolUseID           string `json:"tool_use_id"`
	ToolInput           struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Edits        []struct {
			FilePath string `json:"file_path"`
		} `json:"edits"`
		// SubagentType is the Agent tool's; the task's description and prompt beside it are not decoded.
		SubagentType string `json:"subagent_type"`
		// Command is the Bash tool's, parsed only for the files it writes.
		Command string `json:"command"`
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

// claudeBashTool is the tool_name of Claude Code's shell tool.
const claudeBashTool = "Bash"

// claudeExpectedDir holds the claims Bash calls make before they run.
var claudeExpectedDir = hookrun.AgentStateDir(name, "expected")

// claudeClaims are the claims Claude Code's Bash calls make before they run.
var claudeClaims = shellwrites.Claims{Dir: claudeExpectedDir, Tool: claudeTool}

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
	claudeClaims.Settle(env, r, in.SessionID) // an interrupted last turn had no Stop
	// A turn's last response can reach the transcript after its Stop read it, and an interrupted
	// turn has no Stop.
	captureClaudeCallResponses(env, r, in.SessionID, in.TranscriptPath)
	env.EndSession(r, in.SessionID, claudeTool)
	// A /rename after the last turn ends no turn.
	captureClaudeTitle(env, r, in)
	return nil
}

// stop refreshes account evidence, the session's title and its calls' responses before the
// dispatcher starts delivery.
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
	claudeClaims.Settle(env, r, in.SessionID)
	captureClaudeAccount(env, r, in)
	captureClaudeTitle(env, r, in)
	captureClaudeCallResponses(env, r, in.SessionID, in.TranscriptPath)
	return nil
}

// userPromptSubmit settles the claims of a turn that ended without a Stop: declining a Bash
// call interrupts the turn, and an interrupted turn has no Stop. Its caller
// (internal/cli/hook.go) claims the session and starts the relay. It must print nothing: its
// stdout goes to the model.
//
// Accepted risk: a developer's commit of a file a declined call named, made before the next
// prompt or the session's end, is stamped as the session's. Upgrade path: settle a claim in the
// commit hook when its file is unchanged since the call's PreToolUse.
func userPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		return err
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	if r, err := env.Repo(ctx); err == nil {
		claudeClaims.Settle(env, r, in.SessionID)
	}
	return nil
}

// preToolUse records the files a Bash call writes before its first commit, so that commit is
// stamped: the call's PostToolUse comes after it.
func preToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if in.ToolName != claudeBashTool || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	claudeClaims.BeforeCall(ctx, env, session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}, in.ToolInput.Command)
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
	if in.ToolName == claudeBashTool {
		if written, _ := shellwrites.Writes(in.ToolInput.Command, env.Cwd); len(written) == 0 {
			return nil // most Bash calls write nothing: no repository to resolve
		}
	}
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if isClaudeAgentTool(in.ToolName) {
		claudeSubagentCall(env, r, in)
		return nil
	}
	// Inside a subagent the manifest stays the session's; the event names the agent.
	attrs := hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType)
	hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolUseID)
	sess := session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}
	if in.ToolName == claudeBashTool {
		// Accepted risk: a file a command writes without naming it (a script, a code generator,
		// make) stays unrecorded, so its commit goes unstamped; so does one a background command
		// writes after the turn's Stop settled its unchanged claim. Upgrade path: compare the
		// checkout's changed files before and after the call.
		shellwrites.AfterCall(env, r, sess, in.ToolName, in.ToolInput.Command, attrs)
		return nil
	}
	paths := append([]string{in.ToolInput.FilePath, in.ToolInput.NotebookPath}, editPaths(in)...)
	env.Touch(r, sess, in.ToolName, paths, attrs)
	return nil
}

func editPaths(in *claudeHookInput) []string {
	var out []string
	for _, e := range in.ToolInput.Edits {
		out = append(out, e.FilePath)
	}
	return out
}
