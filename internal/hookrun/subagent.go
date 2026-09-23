package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Subagents come in two shapes and the events keep them apart.
//
// Claude Code and Codex run a subagent inside the parent session: every hook payload
// keeps the parent's session_id and adds agent_id / agent_type, so the subagent is a
// facet of one session. terma.subagent.start / terma.subagent.end bracket it and
// terma.files.touched carries the same agent_id, all under the parent's session id.
//
// Cursor, OpenCode and a Codex thread spawn give the child its own conversation,
// session or rollout. Those are sessions of their own, and their terma.session.start
// names the parent in parent_session_id (OpenCode's Session.parentID, the Codex
// rollout's session_meta.source.subagent.thread_spawn). Cursor reports a subagent's end
// under the parent conversation (subagentStop), which is where its outcome and the
// files it changed are recorded.
//
// None of this is token usage: subagent spend rides the native OTel export (Claude's
// query_source and agent.name), never these events.

// agentAttrs stamps the agent facet on an event when the hook payload names an agent.
// agent_id is the discriminator: Claude Code sends agent_type on every hook of a
// `claude --agent <name>` session, subagent or not, and only agent_id says the hook
// fired inside a subagent — so a type without an id stamps nothing. The id is an
// identifier the harness mints and is held to a session id's charset; the type is a
// name a developer chose for the agent and is only kept single-line and short.
func agentAttrs(attrs map[string]any, id, kind string) map[string]any {
	if !session.ValidID(id) {
		return attrs
	}
	attrs[attrAgentID] = id
	if shortLabel(kind) {
		attrs[attrAgentType] = kind
	}
	return attrs
}

func shortLabel(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "\r\n")
}

// --- Claude Code -----------------------------------------------------------------------

// SubagentStart handles the first half of Claude Code's subagent lifecycle. The payload
// is the parent's (session_id, cwd, prompt_id) plus agent_id and agent_type. The
// subagent's transcript path and last message are in the payload too and are never read.
func SubagentStart(ctx context.Context, env Env) error {
	return claudeSubagent(ctx, env, EventSubagentStart)
}

// SubagentStop handles the second half, from the same payload under the same rule.
func SubagentStop(ctx context.Context, env Env) error {
	return claudeSubagent(ctx, env, EventSubagentEnd)
}

func claudeSubagent(ctx context.Context, env Env, name string) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.AgentID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	attrs := agentAttrs(map[string]any{attrTool: claudeTool, attrSchemaVersion: 1}, in.AgentID, in.AgentType)
	if name == EventSubagentStart {
		attrs[attrVersion] = env.Version
	}
	if session.ValidID(in.PromptID) {
		attrs[attrTurnID] = in.PromptID
	}
	env.emitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: repoName(r.root), Attrs: attrs})
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
func (e Env) claudeSubagentCall(r *repo, in *claudeHookInput) {
	var res claudeAgentResult
	if len(in.ToolResponse) == 0 || json.Unmarshal(in.ToolResponse, &res) != nil {
		e.logf("%s tool without a readable response", in.ToolName)
		return
	}
	if !session.ValidID(res.AgentID) {
		e.logf("%s tool response names no agent", in.ToolName)
		return
	}
	attrs := agentAttrs(map[string]any{attrTool: claudeTool, attrSchemaVersion: 1}, res.AgentID, cmp.Or(res.AgentType, in.ToolInput.SubagentType))
	// A subagent can launch one of its own: the hook then fires inside the launching
	// agent and names it, which is the new agent's parent.
	if session.ValidID(in.AgentID) && in.AgentID != res.AgentID {
		attrs[attrAgentParentID] = in.AgentID
	}
	boundedAttr(attrs, attrModel, res.ResolvedModel)
	boundedAttr(attrs, attrToolCallID, in.ToolUseID)
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
		if shortLabel(label.value) {
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
		if value, _, ok := cursorNumber(raw, true); ok {
			attrs[key] = int64(value)
		}
	}
	e.emitFor(r, spool.Event{Name: EventSubagentCall, SessionID: in.SessionID, Repo: repoName(r.root), Attrs: attrs})
}

// --- Codex -----------------------------------------------------------------------------

// CodexSubagentStart announces a Codex subagent, which is a thread the session spawned.
// Codex sends the root thread's session_id, the child thread's id as agent_id, and the
// child's own rollout as transcript_path (codex-rs/core/src/hook_runtime.rs, read
// 2026-09-17; not yet seen live). The rollout's first line is the spawn record — which
// thread spawned this one, how deep, and Codex's labels for it — and the start carries
// it, with rollout_status saying whether it could be read rather than dropping a miss.
//
// SubagentStop fires at the end of every turn of the child thread, not once: a Codex
// end is per turn, told apart by turn_id, where Claude Code's is a bracket.
func CodexSubagentStart(ctx context.Context, env Env) error {
	return codexSubagent(ctx, env, EventSubagentStart)
}

// CodexSubagentStop is the end of the subagent CodexSubagentStart announced.
func CodexSubagentStop(ctx context.Context, env Env) error {
	return codexSubagent(ctx, env, EventSubagentEnd)
}

func codexSubagent(ctx context.Context, env Env, name string) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.AgentID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	attrs := agentAttrs(map[string]any{attrTool: codexTool, attrSchemaVersion: 1, attrModel: in.Model}, in.AgentID, in.AgentType)
	if session.ValidID(in.TurnID) {
		attrs[attrTurnID] = in.TurnID
	}
	if name == EventSubagentStart {
		attrs[attrVersion] = env.Version
		spawn, status := harness.CodexRolloutSpawn(ctx, in.AgentID, in.TranscriptPath)
		attrs["rollout_status"] = status
		if status == statusPresent {
			codexSpawnAttrs(attrs, attrAgentParentID, spawn)
		}
	}
	env.emitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: repoName(r.root), Attrs: attrs})
	return nil
}

// codexRolloutID is the thread whose rollout a Codex hook's transcript_path names. A
// subagent in Codex is a spawned thread with a rollout of its own: its hooks keep the
// root thread's session_id, put the child thread's id in agent_id, and point
// transcript_path at the child's rollout. Reading that file as the session's fails the
// confined open's own check — the rollout says it is another thread's — so capture
// inside a subagent recorded nothing but a mismatch. Codex names a rollout file after
// its thread, which is what settles it; anything else is the session's, as before.
func codexRolloutID(in *codexHookInput) string {
	if session.ValidID(in.AgentID) && strings.Contains(filepath.Base(in.TranscriptPath), in.AgentID) {
		return in.AgentID
	}
	return in.SessionID
}

// codexSpawnAttrs copies a rollout's spawn record: the thread that spawned this one,
// under parentKey, and Codex's own labels for the child (a random nickname, a
// "/root/<task>" path). The parent is a session when the child is one (a start event
// of its own) and an agent's parent when the child is a facet of the root's session.
func codexSpawnAttrs(attrs map[string]any, parentKey string, spawn harness.CodexThreadSpawn) {
	if !session.ValidID(spawn.ParentThreadID) {
		return
	}
	attrs[parentKey] = spawn.ParentThreadID
	if spawn.Depth > 0 {
		attrs["agent_depth"] = spawn.Depth
	}
	if shortLabel(spawn.AgentNickname) {
		attrs["agent_nickname"] = spawn.AgentNickname
	}
	if shortLabel(spawn.AgentPath) {
		attrs["agent_path"] = spawn.AgentPath
	}
}

// --- Cursor ----------------------------------------------------------------------------

// CursorSubagentStop is Cursor's subagentStop: a subagent finished, and Cursor reports
// its outcome, counts and the files it modified. The event is filed under the
// conversation that spawned it — parent_conversation_id when Cursor sends one — and
// names the subagent's own conversation as agent_id.
//
// The modified files join the parent's manifest. So does any manifest the subagent built
// for itself: cursor-agent can file a subagent's afterFileEdit under the subagent's
// conversation id, and left there the commit would be stamped with a session nobody can
// find, or with two. Folding it in stamps the commit once, for the conversation a person
// can open.
//
// Only subagentStop is wired. Cursor documents that a subagentStart hook which prints
// nothing blocks the subagent, and the committed guard prints nothing on a machine
// without terma: every spawn there would hit that path.
func CursorSubagentStop(ctx context.Context, env Env) error {
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
	own, id := in.id(), in.id()
	if session.ValidID(in.ParentConversationID) {
		id = in.ParentConversationID
	}
	sess := session.Session{ID: id, Tool: cursorTool, Model: in.Model}
	files := relativeFiles(r, env.Cwd, in.ModifiedFiles)
	for _, child := range []string{in.SubagentID, own} {
		if !session.ValidID(child) || child == id {
			continue
		}
		moved, err := r.store.Merge(child, sess, env.now())
		if err != nil {
			env.logf("fold subagent manifest: %v", err)
		}
		files = append(files, moved...)
	}
	files = uniqueSorted(files)

	// The event is a subagent's by definition, so the type stands even when Cursor sent
	// no id to hang it on — the one place agentAttrs' gate does not apply.
	facet := func(attrs map[string]any) map[string]any {
		agentAttrs(attrs, in.SubagentID, in.SubagentType)
		if _, ok := attrs[attrAgentType]; !ok && shortLabel(in.SubagentType) {
			attrs[attrAgentType] = in.SubagentType
		}
		return attrs
	}
	attrs := facet(map[string]any{attrTool: cursorTool, attrSchemaVersion: 1, attrEvidenceSource: sourceCursorHook})
	switch in.Status {
	case "completed", "aborted", "error":
		attrs[attrStatus] = in.Status
	default:
		attrs[attrStatus] = unknownValue
	}
	boundedAttr(attrs, attrTurnID, in.GenerationID)
	for k, v := range map[string]json.RawMessage{"duration_ms": in.DurationMs, "message_count": in.MessageCount, "tool_call_count": in.ToolCallCount, "loop_count": in.LoopCount} {
		if value, _, ok := cursorNumber(v, true); ok {
			attrs[k] = int64(value)
		}
	}
	attrs[attrFileCount] = len(files)
	touched := facet(map[string]any{})
	boundedAttr(touched, attrTurnID, in.GenerationID)
	env.touch(r, sess, "subagentStop", files, touched)
	env.emitFor(r, spool.Event{Name: EventSubagentEnd, SessionID: id, Repo: repoName(r.root), Attrs: attrs})
	return nil
}
