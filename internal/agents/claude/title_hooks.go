package claude

import (
	"slices"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

var claudeTitleStateDir = hookrun.AgentStateDir(name, "titles")

// captureClaudeTitle spools the session's name when new or renamed. It restates the first
// prompt, so it travels under titleConsented and, at delivery, the policy's prompt switch.
func captureClaudeTitle(e hookrun.Env, r *hookrun.Repo, in *claudeHookInput) {
	if in.TranscriptPath == "" || !titleConsented(e.Consent()) {
		return
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeTranscript}
	hookrun.SpoolTitle(e, r, claudeTitleStateDir, in.SessionID, attrs, func() (string, bool, error) {
		return readTranscriptTitle(in.TranscriptPath, in.SessionID)
	})
}

// titleConsented reports whether a title may be sent at all: Claude Code is among the
// developer's agents, which setup points at the relay, or global mode collects every
// session. The team's policy, checked at delivery, decides whether prompts may.
func titleConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}
