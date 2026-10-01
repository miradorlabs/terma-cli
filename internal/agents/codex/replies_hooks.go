package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexReplyMaxText bounds one message: a longer reply is almost always a recited file.
const codexReplyMaxText = 16 << 10

// captureCodexReplies spools the messages recorded since the last capture, under a
// per-session cursor, and only where prompts are consented.
func captureCodexReplies(e hookrun.Env, ctx context.Context, r *hookrun.Repo, in *codexHookInput) {
	pol := routing.EffectivePolicy(e.Policy, r.ProjectID)
	if e.Spool == nil || !session.ValidID(in.SessionID) || !pol.IncludePrompts || !pol.AllowsSignal("logs") || len(pol.ExcludePaths) > 0 || !repliesConsented(r.ProjectID, pol.Global()) {
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
		if _, desktop := codexDesktopRoute(r); desktop {
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
		return e.Spool.Append(spool.Event{Time: at, Name: hookrun.EventAssistantMessage, SessionID: in.SessionID, Repo: r.Name, Workspace: r.Root, Global: pol.Global(), Attrs: attrs})
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

// repliesConsented reports whether prompts, and so replies, may leave for this
// repository. It fails closed: a source that exists and cannot be read might be the one
// that withholds prompts; a missing file is simply not a source.
func repliesConsented(projectID string, global bool) bool {
	rec, recorded, err := routing.LoadRecord(projectID)
	if err != nil {
		return false
	}
	// Under the relay the machine-wide config allows prompts on purpose, so only the project's
	// routing record can consent.
	if claim.Enabled() {
		if global && !recorded {
			return true
		}
		return recorded && rec.IncludePrompts && slices.Contains(rec.Harnesses, name) && slices.Contains(rec.Signals, "logs")
	}
	if slices.Contains(rec.Surfaces, desktop) {
		return recorded && slices.Contains(rec.Harnesses, name) &&
			slices.Contains(rec.Signals, "logs") && rec.IncludePrompts
	}
	st, err := (exporter{}).Status()
	if err != nil {
		return false
	}
	return st.Connected && st.IncludePrompts &&
		(!recorded || !slices.Contains(rec.Harnesses, name) || rec.IncludePrompts)
}
