package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Claude's internal forks mint agent ids and fire SubagentStop without ever
// dispatching SubagentStart or being launched by Agent/Task. Their agent_type can
// inherit the session's --agent, so neither an id nor a type proves delegation.
// Keep launch evidence across hook processes and spool flushes instead.
//
// Each (session, agent) has its own marker, written whole with no read/modify/write
// cycle or shared index. Concurrent launches cannot overwrite one another. The
// repository is not part of the key: a subagent can run in another worktree.
func claudeSubagentPath(sessionID, agentID string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeSubagentDir, EvidenceID(sessionID+"\x00"+agentID)+".json"), nil
}

func (e Env) rememberClaudeSubagent(sessionID, agentID string) {
	if e.Spool == nil {
		return
	}
	path, err := claudeSubagentPath(sessionID, agentID)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = WriteState(path, []byte("{}\n"))
	}
	if err != nil {
		e.Logf("record Claude subagent launch: %v", err)
	}
}

func (e Env) knownClaudeSubagent(sessionID, agentID string) bool {
	path, err := claudeSubagentPath(sessionID, agentID)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	// Keep the marker after a stop: hooks can keep an agent running or it can be
	// resumed. SessionStart prunes old markers; enforce the same age on reads.
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(e.Time().Add(-spool.MaxAge))
}

func (e Env) pruneClaudeSubagents() {
	if dir, err := config.Dir(); err == nil {
		PruneState(filepath.Join(dir, claudeSubagentDir), e.Time().Add(-spool.MaxAge))
	}
}

// SubagentStart handles the first half of Claude Code's subagent lifecycle. The payload
// is the parent's (session_id, cwd, prompt_id) plus agent_id and agent_type. The
// subagent's transcript path and last message are in the payload too and are never read.
func SubagentStart(ctx context.Context, env Env) error {
	return claudeSubagent(ctx, env, EventSubagentStart)
}

// SubagentStop handles the second half for an agent whose launch terma observed.
// Claude's internal forks also fire this hook, without a delegated launch.
func SubagentStop(ctx context.Context, env Env) error {
	return claudeSubagent(ctx, env, EventSubagentEnd)
}

func claudeSubagent(ctx context.Context, env Env, name string) error {
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
	if name == EventSubagentStart {
		env.rememberClaudeSubagent(in.SessionID, in.AgentID)
	} else if !env.knownClaudeSubagent(in.SessionID, in.AgentID) {
		// Internal forks (background summaries, prompt suggestions, /btw) also
		// fire SubagentStop. Only a launch establishes a delegated run.
		return nil
	}
	attrs := AgentAttrs(map[string]any{attrTool: claudeTool, attrSchemaVersion: 1}, in.AgentID, in.AgentType)
	if name == EventSubagentStart {
		attrs[attrVersion] = env.Version
	}
	if session.ValidID(in.PromptID) {
		attrs[attrTurnID] = in.PromptID
	}
	env.EmitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// isClaudeAgentTool reports whether a tool is the one that launches a subagent: "Agent"
// today, "Task" in the builds before it.
func isClaudeAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// claudeAgentResult is the Agent tool's response, as far as terma reads it. The
// response also carries the task's prompt and the subagent's reply (prompt, description,
// content); they are conversation content, have no field here, and so are never decoded.
//
// A subagent launched in the background returns at once with status "async_launched"
// and nothing but its id and model. One the parent waits for returns "completed" with
// the subagent's own account of the run: how long it took, how many tools it used and
// what they did. Every number is optional, and a missing one stays missing rather than
// zero.
//
// WHAT IT DOES NOT HOLD IS WHAT THE RUN SPENT, whatever the field names suggest. `usage`
// is the usage of the subagent's LAST API request, and `totalTokens` is that one
// request's four classes added up — the size the subagent's context had reached when it
// finished. Two live runs, 2026-09-21 (Claude Code 2.1.278), each of two requests
// (input / cache read / cache write): 10/0/13071 then 8/13071/2357, and the response
// said usage 8/13071/2357, totalTokens 15572; 10/0/13060 then 8/13060/2192, and it said
// 8/13060/2192, 15385. The first request is in neither. So `usage` has no field here
// beyond its two labels: its numbers are one request's, which the native export already
// carries under that request's own id, and beside a run's duration they read as the
// run's — the first cut of this event, and the platform adapter written against it, took
// them for exactly that until a reviewer added the requests up.
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

// claudeSubagentCall records the Agent tool call a subagent was launched by. It is the
// only hook that names the subagent's model (SubagentStart does not), and for a
// subagent the parent waited on, the only record of how the run went that is keyed to
// the subagent: how long it took, how many tools it used, what they did, and how large
// its context had grown (final_context_tokens — see claudeAgentResult for why that is
// not what it spent). What a subagent spent is the native export's to say, request by
// request; no hook reports it for a run.
func (e Env) claudeSubagentCall(r *Repo, in *claudeHookInput) {
	var res claudeAgentResult
	if len(in.ToolResponse) == 0 || json.Unmarshal(in.ToolResponse, &res) != nil {
		e.Logf("%s tool without a readable response", in.ToolName)
		return
	}
	if !session.ValidID(res.AgentID) {
		e.Logf("%s tool response names no agent", in.ToolName)
		return
	}
	e.rememberClaudeSubagent(in.SessionID, res.AgentID)
	attrs := AgentAttrs(map[string]any{attrTool: claudeTool, attrSchemaVersion: 1}, res.AgentID, cmp.Or(res.AgentType, in.ToolInput.SubagentType))
	// A subagent can launch one of its own: the hook then fires inside the launching
	// agent and names it, which is the new agent's parent.
	if session.ValidID(in.AgentID) && in.AgentID != res.AgentID {
		attrs[attrAgentParentID] = in.AgentID
	}
	BoundedAttr(attrs, attrModel, res.ResolvedModel)
	BoundedAttr(attrs, attrToolCallID, in.ToolUseID)
	if session.ValidID(in.PromptID) {
		attrs[attrTurnID] = in.PromptID
	}
	switch res.Status {
	case "async_launched", "completed":
		attrs[attrStatus] = res.Status
	default:
		attrs[attrStatus] = unknownValue
	}
	if res.IsAsync != nil {
		attrs["is_async"] = *res.IsAsync
	}
	for _, label := range []struct{ key, value string }{{"service_tier", res.Usage.ServiceTier}, {"speed", res.Usage.Speed}} {
		if ShortLabel(label.value) {
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
		if value, _, ok := JSONNumber(raw, true); ok {
			attrs[key] = int64(value)
		}
	}
	e.EmitFor(r, spool.Event{Name: EventSubagentCall, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
}
