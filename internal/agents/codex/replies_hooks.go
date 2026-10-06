package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"

	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexReplyMaxText bounds one message: a longer reply is almost always a recited file.
const codexReplyMaxText = 16 << 10

// captureCodexReplies spools the messages recorded since the last capture, under a
// per-session cursor, from the turns the team collects; delivery sends them only while the
// team's policy collects prompts. It reports whether the whole rollout is read and every
// turn was collected, which the thread's name waits for.
func captureCodexReplies(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) (collected bool) {
	if e.Spool == nil || !session.ValidID(in.SessionID) || !repliesConsented(e.Consent()) {
		return false
	}
	dir := filepath.Join(e.StateDir, codexReplyCursorDir)
	// A subagent's replies are in its own rollout; see codexRolloutID.
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, hookrun.EvidenceID(rollout)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	var cursor replyCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cursor) != nil {
		e.Logf("invalid reply cursor; replaying rollout")
		cursor = replyCursor{}
	}
	// Inside Stop's three seconds, beside the funding capture's one.
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := readRolloutReplies(ctx, rollout, in.TranscriptPath, cursor, codexReplyMaxText, turnAdmission(ctx, e), func(reply reply) error {
		attrs := hookrun.AgentAttrs(map[string]any{
			semconv.GenAIMainAgentNameKey: codexTool, semconv.TermaEvidenceSourceKey: sourceCodexRollout,
			semconv.TermaMessageIDKey: reply.ID, semconv.TermaMessageTextKey: reply.Text, semconv.TermaMessageTruncatedKey: reply.Truncated,
			hookrun.AttrProjectID: r.ProjectID,
		}, in.AgentID, in.AgentType)
		// Untagged, so the backend files it under the turn's trace id, the one Codex's own
		// turn record carries; tagged desktop, it took the rollout's turn id as a second turn.
		for k, v := range map[string]string{semconv.TermaTurnIDKey: reply.TurnID, semconv.TermaMessagePhaseKey: reply.Phase, semconv.GenAIRequestModelKey: in.Model} {
			hookrun.BoundedAttr(attrs, k, v)
		}
		// Stamped with when the message was said: read at the turn's end, every reply would
		// otherwise sort after the tool calls it introduced.
		at := e.Time()
		if !reply.At.IsZero() && !reply.At.After(at) {
			at = reply.At
		}
		return e.Spool.Append(spool.Event{Time: at, Name: semconv.TermaAssistantMessageEvent, SessionID: in.SessionID, TraceID: reply.TraceID, Repository: r.Repository, Global: e.Policy.Global(), Attrs: attrs})
	})
	if err != nil {
		e.Logf("codex replies (%s): %v", status, err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			if err := hookrun.WriteState(path, b); err != nil {
				e.Logf("reply cursor: %v", err)
			}
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	// A half-written record may be the turn_context that withholds the newest turn.
	return status == "caught_up" && !next.Withheld
}

// repliesConsented reports whether replies may be sent at all: Codex is among the
// developer's agents, which setup points at the relay, or global mode collects every
// session. The team's policy, which its callers check, decides whether prompts may.
func repliesConsented(c hookrun.Consent) bool {
	return c.Global || slices.Contains(c.Agents, name)
}
