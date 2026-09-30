package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func (e Env) captureCodexFunding(ctx context.Context, r *Repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	// Inside a subagent the rollout, and so the cursor into it, is the child thread's.
	// The evidence is still filed under the session, with the agent named.
	rollout := codexRolloutID(in)
	path := filepath.Join(dir, codexFundingCursorDir, EvidenceID(rollout)+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	unlock, err := LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor harness.CodexCursor
	b, cursorErr := os.ReadFile(path)
	if cursorErr == nil && json.Unmarshal(b, &cursor) != nil {
		e.Logf("invalid funding cursor; replaying rollout")
		cursor = harness.CodexCursor{}
	}
	// A session's first capture is when the directory is swept, as for reply cursors:
	// nothing else ever removed a finished session's cursor.
	if os.IsNotExist(cursorErr) {
		defer PruneState(filepath.Dir(path), e.Time().Add(-spool.MaxAge))
	}
	// Resolve the funding owner once (not per evidence): Codex's ChatGPT account, and only when the
	// session is on the subscription route. Empty on the API-key route or when unreadable — never a
	// stale or guessed id.
	accountID, _ := harness.CodexOAuthAccountID()
	// The per-user identity behind a shared Team workspace account_id: Codex's account_id cannot tell
	// members apart, so also resolve the signed-in email and the stable opaque user_id (subscription
	// route only). account_email is emitted raw (the platform's terma-cli identity convention, renamed
	// to user.email downstream); account_user_id is the durable per-user principal the native OTel wire
	// never exposes.
	userEmail, userID, _ := harness.CodexOAuthUser()
	// Keep capture below Codex Stop's three-second timeout.
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, readErr := harness.ReadCodexFunding(ctx, rollout, in.TranscriptPath, cursor, func(ev harness.FundingEvidence) error {
		attrs := AgentAttrs(ev.Attrs, in.AgentID, in.AgentType)
		attrs[AttrTool], attrs[AttrVersion] = codexTool, e.Version
		attrs[AttrEvidenceSource], attrs[AttrEvidenceStatus] = ev.Source, ev.Status
		attrs[AttrSchemaVersion] = 1
		// Stamp the account only onto a real, present ChatGPT quota snapshot. A record with no OpenAI
		// rate-limit evidence (ev.Status != "present": rate_limits:null, or a session run through
		// --oss/--local-provider/a custom model_provider that emits none) is not this ChatGPT account's
		// usage, so it must not carry the id.
		if accountID != "" && ev.Status == StatusPresent {
			attrs[AttrAccountID] = accountID
		}
		// Same gate as account_id: the per-user identity belongs only on a real ChatGPT quota snapshot.
		if userEmail != "" && ev.Status == StatusPresent {
			attrs["account_email"] = userEmail
		}
		if userID != "" && ev.Status == StatusPresent {
			attrs["account_user_id"] = userID
		}
		attrs[AttrProjectID] = r.ProjectID
		r.StampWorktree(attrs)
		if !ev.SourceTime.IsZero() {
			attrs["source_time"] = ev.SourceTime.UTC().Format(time.RFC3339Nano)
		}
		return e.Spool.Append(spool.Event{Time: e.Time(), Name: EventSessionQuota, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	})
	if next != cursor {
		b, _ := json.Marshal(next)
		if err := WriteState(path, b); err != nil {
			e.Logf("funding cursor: %v", err)
		}
	}
	if readErr != nil {
		e.Logf("funding capture: %v", readErr)
	}
	// Capture progress separately: backlog is not a new unavailable quota.
	if status != "caught_up" && status != "append_failed" {
		name := EventSessionCapture
		if status == "not_ready" {
			name = EventSessionQuota
		}
		e.CaptureFunding(r, in.SessionID, codexTool, name, harness.FundingEvidence{Source: sourceCodexRollout, Status: status, Attrs: AgentAttrs(map[string]any{"source_offset": next.Offset}, in.AgentID, in.AgentType)})
	}
}
