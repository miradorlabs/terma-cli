package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// codexReplyMaxText bounds one message's text. A reply is prose; one that runs past this
// is almost always a file the model recited, and the head of it is what a reader wants.
const codexReplyMaxText = 16 << 10

// captureCodexReplies spools the assistant messages Codex has recorded since the last
// capture. It runs at the end of a turn (Stop, and notify for a developer who has no
// repository hooks), holds a cursor per session under a lock the two share, and does
// nothing at all for a developer whose Codex does not export their prompts.
func (e Env) captureCodexReplies(ctx context.Context, r *repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) || !codexRepliesConsented(r) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexReplyCursorDir)
	// A subagent's replies are in its own rollout; see codexRolloutID.
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, evidenceID(rollout)+".json")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, err := lockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor harness.CodexReplyCursor
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &cursor) != nil {
		// Codex's own message ids make the replay the same events, not new ones.
		e.logf("invalid reply cursor; replaying rollout")
		cursor = harness.CodexReplyCursor{}
	}
	// Inside Stop's three seconds, beside the funding capture's one.
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := harness.ReadCodexReplies(ctx, rollout, in.TranscriptPath, cursor, codexReplyMaxText, func(reply harness.CodexReply) error {
		attrs := agentAttrs(map[string]any{
			attrTool: codexTool, attrSchemaVersion: 1, attrEvidenceSource: sourceCodexRollout,
			"message_id": reply.ID, "role": "assistant",
			"text": reply.Text, "text_bytes": reply.Bytes, "text_truncated": reply.Truncated,
			attrVersion: e.Version, AttrProjectID: r.projectID,
		}, in.AgentID, in.AgentType)
		for k, v := range map[string]string{attrTurnID: reply.TurnID, "trace_id": reply.TraceID, "phase": reply.Phase, attrModel: in.Model} {
			boundedAttr(attrs, k, v)
		}
		// The event is stamped with when the message was said. Every reply of a turn is
		// read at its end, and a chain ordered by when terma read them would put each
		// after the tool calls it introduced.
		at := e.now()
		if !reply.At.IsZero() && !reply.At.After(at) {
			at = reply.At
		}
		return e.Spool.Append(spool.Event{Time: at, Name: EventAssistantMessage, SessionID: in.SessionID, Repo: repoName(r.root), Attrs: attrs})
	})
	if err != nil {
		e.logf("codex replies (%s): %v", status, err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			if err := writeState(path, b); err != nil {
				e.logf("reply cursor: %v", err)
			}
		}
	}
	if os.IsNotExist(readErr) {
		pruneQuotaState(dir, e.now().Add(-spool.MaxAge))
	}
}

// codexRepliesConsented reports whether this developer's Codex exports their prompts in
// this repository — the consent a reply travels under. `terma install --exclude-prompts`
// and `terma connect codex --exclude-prompts` are documented as withholding "prompt text
// or model responses", and this is the model-response half of that promise.
//
// A routed CLI launch is marked by the shim; its runtime overrides govern consent.
// Desktop launches have no marker. When the machine-wide exporter points at the
// desktop relay, both that exporter and this repository's desktop route must allow
// prompts; otherwise the reply would bypass the relay's per-repository filter.
//
// It fails closed. A configuration that is there and cannot be read — a routing record
// half-written, a config.toml that does not parse — might be the one that withholds
// prompts, and "could not tell" is not consent. A file that does not exist is different:
// both loaders report that without an error, and it simply is not a source.
func codexRepliesConsented(r *repo) bool {
	rec, recorded, err := shim.LoadRecord(r.projectID)
	if err != nil {
		return false
	}
	st, err := (harness.Codex{}).Status()
	if err != nil {
		return false
	}
	if os.Getenv(shim.CodexRoutedEnv) == "1" {
		return recorded && slices.Contains(rec.Harnesses, shim.AgentCodex) && rec.IncludePrompts
	}
	if st.Endpoint == desktoprelay.Endpoint {
		return st.Connected && st.IncludePrompts && recorded &&
			slices.Contains(rec.Harnesses, shim.AgentCodex) &&
			(rec.Desktop == nil || *rec.Desktop) && slices.Contains(rec.Signals, "logs") &&
			rec.IncludePrompts
	}
	// Repository hooks can run even when an IDE or TERMA_DISABLE bypasses the
	// shim. Keep a saved repository opt-out in force for those launches.
	return st.Connected && st.IncludePrompts &&
		(!recorded || !slices.Contains(rec.Harnesses, shim.AgentCodex) || rec.IncludePrompts)
}
