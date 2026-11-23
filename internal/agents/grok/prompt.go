package grok

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// promptMaxBytes bounds a prompt's text, as a reply's is.
const promptMaxBytes = 16 << 10

// userPromptSubmit claims the session and spools the prompt as terma.user.message: grok's
// export loses a headless run's prompt when it beats the export's start, and this hook always
// fires. Its id is the hook's promptId, which nothing in the export joins, so it names no
// turn. The text travels only while grok is the developer's (ContentConsented) and the team's
// policy collects prompts, cut on a character's edge; a prompt without it still counts. A
// subagent's prompt, which its parent agent wrote, names the subagent's kind. It prints
// nothing: grok hands this hook's stdout to the model.
func userPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	// With no id the message could not be told from another; dispatch still claims the session.
	if !session.ValidID(in.PromptID) {
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaMessageIDKey: in.PromptID}
	if in.child() && hookrun.ShortLabel(in.SubagentType) {
		attrs[semconv.GenAIAgentNameKey] = in.SubagentType
	}
	if prompts, _ := env.Policy.Content(); prompts && (Agent{}).ContentConsented(env.Consent()) {
		text := hookrun.TruncateBytes(in.Prompt, promptMaxBytes)
		attrs[semconv.TermaMessageTextKey] = text
		attrs[semconv.TermaMessageTruncatedKey] = len(text) < len(in.Prompt)
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaUserMessageEvent, SessionID: in.SessionID, Attrs: attrs})
	return nil
}
