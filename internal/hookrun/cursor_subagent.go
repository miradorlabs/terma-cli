package hookrun

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = in.cwd(env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	own, id := in.id(), in.id()
	if session.ValidID(in.ParentConversationID) {
		id = in.ParentConversationID
	}
	sess := session.Session{ID: id, Tool: cursorTool, Model: in.Model}
	files := RelativeFiles(r, env.Cwd, in.ModifiedFiles)
	for _, child := range []string{in.SubagentID, own} {
		if !session.ValidID(child) || child == id {
			continue
		}
		moved, err := r.Store.Merge(child, sess, env.Time())
		if err != nil {
			env.Logf("fold subagent manifest: %v", err)
		}
		files = append(files, moved...)
	}
	files = UniqueSorted(files)

	// The event is a subagent's by definition, so the type stands even when Cursor sent
	// no id to hang it on — the one place agentAttrs' gate does not apply.
	facet := func(attrs map[string]any) map[string]any {
		AgentAttrs(attrs, in.SubagentID, in.SubagentType)
		if _, ok := attrs[AttrAgentType]; !ok && ShortLabel(in.SubagentType) {
			attrs[AttrAgentType] = in.SubagentType
		}
		return attrs
	}
	attrs := facet(map[string]any{AttrTool: cursorTool, AttrSchemaVersion: 1, AttrEvidenceSource: sourceCursorHook})
	switch in.Status {
	case "completed", "aborted", "error":
		attrs[AttrStatus] = in.Status
	default:
		attrs[AttrStatus] = UnknownValue
	}
	BoundedAttr(attrs, AttrTurnID, in.GenerationID)
	for k, v := range map[string]json.RawMessage{"duration_ms": in.DurationMs, "message_count": in.MessageCount, "tool_call_count": in.ToolCallCount, "loop_count": in.LoopCount} {
		if value, _, ok := JSONNumber(v, true); ok {
			attrs[k] = int64(value)
		}
	}
	attrs[AttrFileCount] = len(files)
	touched := facet(map[string]any{})
	BoundedAttr(touched, AttrTurnID, in.GenerationID)
	env.Touch(r, sess, "subagentStop", files, touched)
	env.EmitFor(r, spool.Event{Name: EventSubagentEnd, SessionID: id, Repo: r.Name, Attrs: attrs})
	return nil
}
