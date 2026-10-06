package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// claudeSubagentDir holds launch evidence per (session, agent).
var claudeSubagentDir = hookrun.AgentStateDir(name, "subagents")

// Claude Code's internal forks mint agent ids and fire SubagentStop without a launch, so only a
// recorded launch proves delegation. One marker per (session, agent), written whole, so concurrent
// launches never overwrite one another; the repository is not in the key, since a subagent can run
// in another worktree.
func claudeSubagentPath(dir, sessionID, agentID string) string {
	return filepath.Join(dir, claudeSubagentDir, hookrun.EvidenceID(sessionID+"\x00"+agentID)+".json")
}

func rememberClaudeSubagent(e hookrun.Env, sessionID, agentID string) {
	if e.Spool == nil {
		return
	}
	path := claudeSubagentPath(e.StateDir, sessionID, agentID)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = hookrun.WriteState(path, []byte("{}\n"))
	}
	if err != nil {
		e.Logf("record Claude subagent launch: %v", err)
	}
}

func knownClaudeSubagent(e hookrun.Env, sessionID, agentID string) bool {
	info, err := os.Lstat(claudeSubagentPath(e.StateDir, sessionID, agentID))
	// Kept after a stop, since an agent can be resumed; aged out like the spool.
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(e.Time().Add(-spool.MaxAge))
}

func pruneClaudeSubagents(e hookrun.Env) {
	hookrun.PruneState(filepath.Join(e.StateDir, claudeSubagentDir), e.Time().Add(-spool.MaxAge))
}

// subagentStart handles SubagentStart; the transcript path and last message are never read.
func subagentStart(ctx context.Context, env hookrun.Env) error {
	return claudeSubagent(ctx, env, semconv.TermaSubagentStartEvent)
}

// subagentStop handles SubagentStop for an agent whose launch terma observed.
func subagentStop(ctx context.Context, env hookrun.Env) error {
	return claudeSubagent(ctx, env, semconv.TermaSubagentEndEvent)
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
	if name == semconv.TermaSubagentStartEvent {
		rememberClaudeSubagent(env, in.SessionID, in.AgentID)
	} else if !knownClaudeSubagent(env, in.SessionID, in.AgentID) {
		// Internal forks (background summaries, prompt suggestions, /btw) fire it too.
		return nil
	}
	attrs := hookrun.AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: claudeTool}, in.AgentID, in.AgentType)
	if session.ValidID(in.PromptID) {
		attrs[semconv.TermaTurnIDKey] = in.PromptID
	}
	env.EmitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Attrs: attrs})
	return nil
}

// isClaudeAgentTool reports whether a tool launches a subagent: "Agent", or "Task" in older builds.
func isClaudeAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// claudeAgentResult is the Agent tool's response as far as terma reads it; the prompt,
// description and reply have no field, so they are never decoded. The duration stays missing
// rather than zero when absent.
type claudeAgentResult struct {
	Status          string          `json:"status"`
	AgentID         string          `json:"agentId"`
	AgentType       string          `json:"agentType"`
	ResolvedModel   string          `json:"resolvedModel"`
	TotalDurationMs json.RawMessage `json:"totalDurationMs"`
}

// claudeSubagentCall records the Agent tool call that launched a subagent: the only hook naming its
// model and, when the parent waited, how long the run took.
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
	attrs := hookrun.AgentAttrs(map[string]any{semconv.GenAIMainAgentNameKey: claudeTool}, res.AgentID, cmp.Or(res.AgentType, in.ToolInput.SubagentType))
	// A subagent can launch one of its own; the hook then names the launching agent, the new one's parent.
	if session.ValidID(in.AgentID) && in.AgentID != res.AgentID {
		attrs[semconv.TermaAgentParentIDKey] = in.AgentID
	}
	hookrun.BoundedAttr(attrs, semconv.GenAIRequestModelKey, res.ResolvedModel)
	hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolUseID)
	if session.ValidID(in.PromptID) {
		attrs[semconv.TermaTurnIDKey] = in.PromptID
	}
	switch res.Status {
	case "async_launched", "completed":
		attrs[semconv.TermaOperationStatusKey] = res.Status
	default:
		attrs[semconv.TermaOperationStatusKey] = hookrun.UnknownValue
	}
	if value, _, ok := hookrun.JSONNumber(res.TotalDurationMs, true); ok {
		attrs[semconv.TermaOperationDurationMsKey] = int64(value)
	}
	e.EmitFor(r, spool.Event{Name: semconv.TermaSubagentCallEvent, SessionID: in.SessionID, Attrs: attrs})
}
