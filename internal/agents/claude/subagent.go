package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// claudeSubagentDir holds launch evidence per (session, agent).
const claudeSubagentDir = "claude-subagents"

// Claude Code's internal forks mint agent ids and fire SubagentStop without a launch, so only a
// recorded launch proves delegation. One marker per (session, agent), written whole, so concurrent
// launches never overwrite one another; the repository is not in the key, since a subagent can run
// in another worktree.
func claudeSubagentPath(sessionID, agentID string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeSubagentDir, hookrun.EvidenceID(sessionID+"\x00"+agentID)+".json"), nil
}

func rememberClaudeSubagent(e hookrun.Env, sessionID, agentID string) {
	if e.Spool == nil {
		return
	}
	path, err := claudeSubagentPath(sessionID, agentID)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = hookrun.WriteState(path, []byte("{}\n"))
	}
	if err != nil {
		e.Logf("record Claude subagent launch: %v", err)
	}
}

func knownClaudeSubagent(e hookrun.Env, sessionID, agentID string) bool {
	path, err := claudeSubagentPath(sessionID, agentID)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	// Kept after a stop, since an agent can be resumed; aged out like the spool.
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(e.Time().Add(-spool.MaxAge))
}

func pruneClaudeSubagents(e hookrun.Env) {
	if dir, err := config.Dir(); err == nil {
		hookrun.PruneState(filepath.Join(dir, claudeSubagentDir), e.Time().Add(-spool.MaxAge))
	}
}

// subagentStart handles SubagentStart; the transcript path and last message are never read.
func subagentStart(ctx context.Context, env hookrun.Env) error {
	return claudeSubagent(ctx, env, hookrun.EventSubagentStart)
}

// subagentStop handles SubagentStop for an agent whose launch terma observed.
func subagentStop(ctx context.Context, env hookrun.Env) error {
	return claudeSubagent(ctx, env, hookrun.EventSubagentEnd)
}

func claudeSubagent(ctx context.Context, env hookrun.Env, name string) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.AgentID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if name == hookrun.EventSubagentStart {
		rememberClaudeSubagent(env, in.SessionID, in.AgentID)
	} else if !knownClaudeSubagent(env, in.SessionID, in.AgentID) {
		// Internal forks (background summaries, prompt suggestions, /btw) fire it too.
		return nil
	}
	attrs := hookrun.AgentAttrs(map[string]any{hookrun.AttrTool: claudeTool, hookrun.AttrSchemaVersion: 1}, in.AgentID, in.AgentType)
	if name == hookrun.EventSubagentStart {
		attrs[hookrun.AttrVersion] = env.Version
	}
	if session.ValidID(in.PromptID) {
		attrs[hookrun.AttrTurnID] = in.PromptID
	}
	env.EmitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// isClaudeAgentTool reports whether a tool launches a subagent: "Agent", or "Task" in older builds.
func isClaudeAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// claudeAgentResult is the Agent tool's response as far as terma reads it; the prompt,
// description and reply have no field, so they are never decoded. Every number stays missing
// rather than zero when absent. `usage` and `totalTokens` describe the subagent's last API request
// (the context's final size), not what the run spent, so only usage's two labels are read.
type claudeAgentResult struct {
	Status            string          `json:"status"`
	IsAsync           *bool           `json:"isAsync"`
	AgentID           string          `json:"agentId"`
	AgentType         string          `json:"agentType"`
	ResolvedModel     string          `json:"resolvedModel"`
	TotalDurationMs   json.RawMessage `json:"totalDurationMs"`
	TotalTokens       json.RawMessage `json:"totalTokens"`
	TotalToolUseCount json.RawMessage `json:"totalToolUseCount"`
	Usage             struct {
		ServiceTier string `json:"service_tier"`
		Speed       string `json:"speed"`
	} `json:"usage"`
	ToolStats struct {
		ReadCount      json.RawMessage `json:"readCount"`
		SearchCount    json.RawMessage `json:"searchCount"`
		BashCount      json.RawMessage `json:"bashCount"`
		EditFileCount  json.RawMessage `json:"editFileCount"`
		LinesAdded     json.RawMessage `json:"linesAdded"`
		LinesRemoved   json.RawMessage `json:"linesRemoved"`
		OtherToolCount json.RawMessage `json:"otherToolCount"`
	} `json:"toolStats"`
}

// claudeSubagentCall records the Agent tool call that launched a subagent: the only hook naming its
// model and, when the parent waited, how the run went. final_context_tokens is a size, never a spend.
func claudeSubagentCall(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	var res claudeAgentResult
	if len(in.ToolResponse) == 0 || json.Unmarshal(in.ToolResponse, &res) != nil {
		e.Logf("%s tool without a readable response", in.ToolName)
		return
	}
	if !session.ValidID(res.AgentID) {
		e.Logf("%s tool response names no agent", in.ToolName)
		return
	}
	rememberClaudeSubagent(e, in.SessionID, res.AgentID)
	attrs := hookrun.AgentAttrs(map[string]any{hookrun.AttrTool: claudeTool, hookrun.AttrSchemaVersion: 1}, res.AgentID, cmp.Or(res.AgentType, in.ToolInput.SubagentType))
	// A subagent can launch one of its own; the hook then names the launching agent, the new one's parent.
	if session.ValidID(in.AgentID) && in.AgentID != res.AgentID {
		attrs[hookrun.AttrAgentParentID] = in.AgentID
	}
	hookrun.BoundedAttr(attrs, hookrun.AttrModel, res.ResolvedModel)
	hookrun.BoundedAttr(attrs, hookrun.AttrToolCallID, in.ToolUseID)
	if session.ValidID(in.PromptID) {
		attrs[hookrun.AttrTurnID] = in.PromptID
	}
	switch res.Status {
	case "async_launched", "completed":
		attrs[hookrun.AttrStatus] = res.Status
	default:
		attrs[hookrun.AttrStatus] = hookrun.UnknownValue
	}
	if res.IsAsync != nil {
		attrs["is_async"] = *res.IsAsync
	}
	for _, label := range []struct{ key, value string }{{"service_tier", res.Usage.ServiceTier}, {"speed", res.Usage.Speed}} {
		if hookrun.ShortLabel(label.value) {
			attrs[label.key] = label.value
		}
	}
	for key, raw := range map[string]json.RawMessage{
		"duration_ms":          res.TotalDurationMs,
		"final_context_tokens": res.TotalTokens,
		"tool_call_count":      res.TotalToolUseCount,
		"read_count":           res.ToolStats.ReadCount,
		"search_count":         res.ToolStats.SearchCount,
		"bash_count":           res.ToolStats.BashCount,
		"edit_file_count":      res.ToolStats.EditFileCount,
		"lines_added":          res.ToolStats.LinesAdded,
		"lines_removed":        res.ToolStats.LinesRemoved,
		"other_tool_count":     res.ToolStats.OtherToolCount,
	} {
		if value, _, ok := hookrun.JSONNumber(raw, true); ok {
			attrs[key] = int64(value)
		}
	}
	e.EmitFor(r, spool.Event{Name: hookrun.EventSubagentCall, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
}
