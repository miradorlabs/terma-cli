package cursor

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// subagentStop files a finished subagent under the conversation that spawned it, naming
// its own as terma.agent.id. Its files and any manifest it built under its own id join the
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
	files := in.ModifiedFiles
	for _, child := range []string{in.SubagentID, own} {
		if !session.ValidID(child) || child == id {
			continue
		}
		moved, err := r.Store.Merge(child, sess, env.Time())
		if err != nil {
			env.Logf("fold subagent manifest: %v", err)
		}
		// A manifest names its files from the checkout's root, not the hook's cwd.
		for _, f := range moved {
			files = append(files, filepath.Join(r.Root, filepath.FromSlash(f)))
		}
	}

	// The event is a subagent's by definition, so the type stands without an id.
	facet := func(attrs map[string]any) map[string]any {
		hookrun.AgentAttrs(attrs, in.SubagentID, in.SubagentType)
		if _, ok := attrs[semconv.GenAIAgentNameKey]; !ok && hookrun.ShortLabel(in.SubagentType) {
			attrs[semconv.GenAIAgentNameKey] = in.SubagentType
		}
		return attrs
	}
	attrs := facet(map[string]any{semconv.GenAIMainAgentNameKey: cursorTool, semconv.TermaEvidenceSourceKey: sourceCursorHook})
	switch in.Status {
	case "completed", "aborted", "error":
		attrs[semconv.TermaOperationStatusKey] = in.Status
	default:
		attrs[semconv.TermaOperationStatusKey] = hookrun.UnknownValue
	}
	hookrun.BoundedAttr(attrs, semconv.TermaTurnIDKey, in.GenerationID)
	for k, v := range map[string]json.RawMessage{
		semconv.TermaOperationDurationMsKey: in.DurationMs, semconv.TermaMessageCountKey: in.MessageCount,
		semconv.TermaToolCallCountKey: in.ToolCallCount, semconv.TermaLoopCountKey: in.LoopCount,
	} {
		if value, _, ok := hookrun.JSONNumber(v, true); ok {
			attrs[k] = int64(value)
		}
	}
	touched := facet(map[string]any{})
	hookrun.BoundedAttr(touched, semconv.TermaTurnIDKey, in.GenerationID)
	env.Touch(r, sess, "subagentStop", files, touched)
	env.EmitFor(r, spool.Event{Name: semconv.TermaSubagentEndEvent, SessionID: id, Attrs: attrs})
	return nil
}
