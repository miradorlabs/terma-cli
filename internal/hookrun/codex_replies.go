package hookrun

import (
	"context"
	"encoding/json"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexReplyMaxText bounds one message's text. A reply is prose; one that runs past this
// is almost always a file the model recited, and the head of it is what a reader wants.
const codexReplyMaxText = 16 << 10

// captureCodexReplies spools the assistant messages Codex has recorded since the last
// capture. It runs at the end of a turn (Stop, and notify for a developer who has no
// repository hooks), holds a cursor per session under a lock the two share, and does
// nothing at all for a developer whose Codex does not export their prompts.
func (e Env) captureCodexReplies(ctx context.Context, r *Repo, in *codexHookInput) {
	pol := routing.EffectivePolicy(e.Policy, r.ProjectID)
	if e.Spool == nil || !session.ValidID(in.SessionID) || !pol.IncludePrompts || !pol.AllowsSignal("logs") || len(pol.ExcludePaths) > 0 || !CodexRepliesConsented(r.ProjectID, pol.Global()) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexReplyCursorDir)
	// A subagent's replies are in its own rollout; see codexRolloutID.
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, EvidenceID(rollout)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor harness.CodexReplyCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cursor) != nil {
		// Codex's own message ids make the replay the same events, not new ones.
		e.Logf("invalid reply cursor; replaying rollout")
		cursor = harness.CodexReplyCursor{}
	}
	// Inside Stop's three seconds, beside the funding capture's one.
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := harness.ReadCodexReplies(ctx, rollout, in.TranscriptPath, cursor, codexReplyMaxText, func(reply harness.CodexReply) error {
		attrs := AgentAttrs(map[string]any{
			attrTool: codexTool, attrSchemaVersion: 1, attrEvidenceSource: sourceCodexRollout,
			"message_id": reply.ID, "role": "assistant",
			"text": reply.Text, "text_bytes": reply.Bytes, "text_truncated": reply.Truncated,
			attrVersion: e.Version, AttrProjectID: r.ProjectID,
		}, in.AgentID, in.AgentType)
		r.StampWorktree(attrs)
		if _, desktop := codexDesktopRoute(r); desktop {
			attrs["capture_surface"] = codexDesktopSurface
		}
		for k, v := range map[string]string{attrTurnID: reply.TurnID, "trace_id": reply.TraceID, "phase": reply.Phase, attrModel: in.Model} {
			BoundedAttr(attrs, k, v)
		}
		// The event is stamped with when the message was said. Every reply of a turn is
		// read at its end, and a chain ordered by when terma read them would put each
		// after the tool calls it introduced.
		at := e.Time()
		if !reply.At.IsZero() && !reply.At.After(at) {
			at = reply.At
		}
		return e.Spool.Append(spool.Event{Time: at, Name: EventAssistantMessage, SessionID: in.SessionID, Repo: r.Name, Workspace: r.Root, Global: pol.Global(), Attrs: attrs})
	})
	if err != nil {
		e.Logf("codex replies (%s): %v", status, err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			if err := WriteState(path, b); err != nil {
				e.Logf("reply cursor: %v", err)
			}
		}
	}
	if os.IsNotExist(readErr) {
		PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// CodexRepliesConsented reports whether this developer's Codex exports their prompts in
// this repository — the consent a reply travels under. `terma install --exclude-prompts`
// and `terma connect codex --exclude-prompts` are documented as withholding "prompt text
// or model responses", and this is the model-response half of that promise.
//
// Relay and Desktop configurations use the repository's routing record. Legacy
// native configurations use Codex's settings. Capture and queued delivery share this
// check; the caller also applies the current organization policy ceiling.
//
// It fails closed. A configuration that is there and cannot be read — a routing record
// half-written, a config.toml that does not parse — might be the one that withholds
// prompts, and "could not tell" is not consent. A file that does not exist is different:
// both loaders report that without an error, and it simply is not a source.
func CodexRepliesConsented(projectID string, global bool) bool {
	rec, recorded, err := routing.LoadRecord(projectID)
	if err != nil {
		return false
	}
	// Through the local relay the machine-wide Codex config lets prompts out on
	// purpose — the relay withholds them per project — so it says nothing about this
	// repository. Only the project's own routing record can consent.
	if claim.Enabled() {
		if global && !recorded {
			return true
		}
		return recorded && rec.IncludePrompts && slices.Contains(rec.Harnesses, routing.AgentCodex) && slices.Contains(rec.Signals, "logs")
	}
	if rec.Desktop {
		return recorded && slices.Contains(rec.Harnesses, routing.AgentCodex) &&
			slices.Contains(rec.Signals, "logs") && rec.IncludePrompts
	}
	st, err := (harness.Codex{}).Status()
	if err != nil {
		return false
	}
	// A saved repository opt-out stays in force whatever the machine-wide config says.
	return st.Connected && st.IncludePrompts &&
		(!recorded || !slices.Contains(rec.Harnesses, routing.AgentCodex) || rec.IncludePrompts)
}
