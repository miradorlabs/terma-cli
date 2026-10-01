package cursor

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// subagentStop files a finished subagent under the conversation that spawned it, naming
// its own as agent_id. Its files and any manifest it built under its own id join the
// parent's, so the commit is stamped once. subagentStart is never wired: a hook that
// prints nothing blocks the spawn.
func subagentStop(ctx context.Context, env hookrun.Env) error {
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
	files := hookrun.RelativeFiles(r, env.Cwd, in.ModifiedFiles)
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
	files = hookrun.UniqueSorted(files)

	// The event is a subagent's by definition, so the type stands without an id.
	facet := func(attrs map[string]any) map[string]any {
		hookrun.AgentAttrs(attrs, in.SubagentID, in.SubagentType)
		if _, ok := attrs[hookrun.AttrAgentType]; !ok && hookrun.ShortLabel(in.SubagentType) {
			attrs[hookrun.AttrAgentType] = in.SubagentType
		}
		return attrs
	}
	attrs := facet(map[string]any{hookrun.AttrTool: cursorTool, hookrun.AttrSchemaVersion: 1, hookrun.AttrEvidenceSource: sourceCursorHook})
	switch in.Status {
	case "completed", "aborted", "error":
		attrs[hookrun.AttrStatus] = in.Status
	default:
		attrs[hookrun.AttrStatus] = hookrun.UnknownValue
	}
	hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, in.GenerationID)
	for k, v := range map[string]json.RawMessage{"duration_ms": in.DurationMs, "message_count": in.MessageCount, "tool_call_count": in.ToolCallCount, "loop_count": in.LoopCount} {
		if value, _, ok := hookrun.JSONNumber(v, true); ok {
			attrs[k] = int64(value)
		}
	}
	attrs[hookrun.AttrFileCount] = len(files)
	touched := facet(map[string]any{})
	hookrun.BoundedAttr(touched, hookrun.AttrTurnID, in.GenerationID)
	env.Touch(r, sess, "subagentStop", files, touched)
	env.EmitFor(r, spool.Event{Name: hookrun.EventSubagentEnd, SessionID: id, Repo: r.Name, Attrs: attrs})
	return nil
}
