package grok

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// subagentStart announces the subagent grok is about to run as a session of its own. grok
// fires it in the parent: session_id is the parent and subagentId the child, whose first
// record and first hook come after it. It claims the child with the parent named, for the
// relay to stamp on the child's records (grok's export links nothing), and spools the child's
// start with its parent and kind. The child is never made active.
func subagentStart(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.SubagentID) || in.SubagentID == in.SessionID {
		return nil
	}
	r, ok := env.Open(ctx, in.SubagentID, in.Cwd)
	if !ok {
		return nil
	}
	var extra map[string]any
	if hookrun.ShortLabel(in.SubagentType) {
		extra = map[string]any{semconv.GenAIAgentNameKey: in.SubagentType}
	}
	env.StartSubagent(r, env.NewSession(r, in.SubagentID, name, ""), in.SessionID, true, extra)
	return nil
}
