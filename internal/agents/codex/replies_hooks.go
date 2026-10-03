package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexReplyMaxText bounds one message: a longer reply is almost always a recited file.
const codexReplyMaxText = 16 << 10

// captureCodexReplies spools the messages recorded since the last capture, under a
// per-session cursor, where this repository routes Codex; delivery sends them only while
// the team's policy collects prompts.
func captureCodexReplies(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) || !repliesConsented(r.Consent(e.Policy.Global())) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexReplyCursorDir)
	// A subagent's replies are in its own rollout; see codexRolloutID.
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, hookrun.EvidenceID(rollout)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
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
	next, status, err := readRolloutReplies(ctx, rollout, in.TranscriptPath, cursor, codexReplyMaxText, func(reply reply) error {
		attrs := hookrun.AgentAttrs(map[string]any{
			hookrun.AttrTool: codexTool, hookrun.AttrSchemaVersion: 1, hookrun.AttrEvidenceSource: sourceCodexRollout,
			"message_id": reply.ID, "role": "assistant",
			"text": reply.Text, "text_bytes": reply.Bytes, "text_truncated": reply.Truncated,
			hookrun.AttrVersion: e.Version, hookrun.AttrProjectID: r.ProjectID,
		}, in.AgentID, in.AgentType)
		r.StampWorktree(attrs)
		if codexDesktopRoute(r) {
			attrs["capture_surface"] = codexDesktopSurface
		}
		for k, v := range map[string]string{hookrun.AttrTurnID: reply.TurnID, "trace_id": reply.TraceID, "phase": reply.Phase, hookrun.AttrModel: in.Model} {
			hookrun.BoundedAttr(attrs, k, v)
		}
		// Stamped with when the message was said: read at the turn's end, every reply would
		// otherwise sort after the tool calls it introduced.
		at := e.Time()
		if !reply.At.IsZero() && !reply.At.After(at) {
			at = reply.At
		}
		return e.Spool.Append(spool.Event{Time: at, Name: hookrun.EventAssistantMessage, SessionID: in.SessionID, Repo: r.Name, Global: e.Policy.Global(), Attrs: attrs})
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
}

// repliesConsented reports whether replies may be sent for this repository at all: the
// team's policy, which its callers check, decides whether prompts may. It fails closed: a
// source that exists and cannot be read might be the one that withholds them; a missing
// file is simply not a source.
func repliesConsented(c hookrun.Consent) bool {
	rec, recorded := c.Route, c.Recorded
	if c.RouteErr != nil {
		return false
	}
	// Under the relay the machine-wide config sends everything on purpose, so only the
	// project's routing record can route the replies here.
	if c.Relay {
		if c.Global && !recorded {
			return true
		}
		return recorded && slices.Contains(rec.Harnesses, name) && slices.Contains(rec.Signals, "logs")
	}
	if slices.Contains(rec.Surfaces, name) {
		return recorded && slices.Contains(rec.Harnesses, name) && slices.Contains(rec.Signals, "logs")
	}
	st, err := (exporter{}).Status()
	return err == nil && st.Connected
}
