package grok

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// promptMaxBytes bounds a prompt's text, as a reply's is.
const promptMaxBytes = 16 << 10

// uuidShape is the shape of the ids grok mints: a session's, a subagent's being its session's,
// and the id of a prompt someone wrote, a person or a parent agent.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// grokTurnPrefixes begin the prompt ids grok gives a turn it starts itself, with no prompt
// anyone wrote: a background task or subagent that finished, a message between a parent agent
// and its child, a workflow's end, notifications, a goal's summary or check, a nudge, a plan
// resumed (grok 1.0.92, its prompt_origin list; subagent-completed-<child session> seen live).
var grokTurnPrefixes = []string{
	"task-completed-", "subagent-completed-", "parent-agent-message-", "parent-message-",
	"workflow-completed-", "notifications-", "goal-summary-", "goal-classifier-", "nudge-",
	"plan-resume-",
}

// userPromptSubmit claims the session and spools the prompt as terma.user.message: grok's
// export loses a headless run's prompt when it beats the export's start, and this hook always
// fires. The text travels only while grok is the developer's (ContentConsented) and the team's
// policy collects prompts, cut on a character's edge; a prompt without it still counts. A
// subagent's prompt, which its parent agent wrote, names the subagent's kind. grok also fires
// this hook for a turn it starts itself (grokTurnPrefixes): nobody's prompt, and grok's export
// sends no user_prompt for it, so it is not spooled. It prints nothing: grok hands this hook's
// stdout to the model.
func userPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	// With no id the message could not be told from another, and a turn grok started itself is
	// no prompt; dispatch still claims the session.
	id := messageID(in)
	if id == "" {
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: name, semconv.TermaMessageIDKey: id}
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

// messageID is the id a prompt is spooled under, "" for none. Nothing in the export joins the
// hook's promptId, so it names no turn; it only has to tell one prompt from another in its
// session, which is how the platform keys it. A UUID is grok's own, new for every prompt: the
// terminal's, a headless run's, and an editor's (ACP) that sent none. An editor may send its
// own id instead, in session/prompt's _meta.promptId, and grok runs the hook with it as it is,
// a repeat in the same session included (T3 Code's t3-xai-prompt-<n>; grok 1.0.92). Such an id
// is turned into one this prompt alone has: a hash of the session, the id and the hook's time,
// shaped as a UUID. A turn grok starts itself has none.
func messageID(in *grokHookInput) string {
	switch {
	case in.PromptID == "":
		return ""
	case uuidShape.MatchString(in.PromptID):
		return in.PromptID
	}
	for _, p := range grokTurnPrefixes {
		if strings.HasPrefix(in.PromptID, p) {
			return ""
		}
	}
	seed := in.Timestamp
	if seed == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		seed = fmt.Sprintf("%x", b)
	}
	sum := sha256.Sum256([]byte(in.SessionID + "\x00" + in.PromptID + "\x00" + seed))
	sum[6] = sum[6]&0x0f | 0x80 // version 8: custom
	sum[8] = sum[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
