package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func captureCodexFunding(e hookrun.Env, ctx context.Context, r *hookrun.Repo, in *codexHookInput) {
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
	path := filepath.Join(dir, codexFundingCursorDir, hookrun.EvidenceID(rollout)+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor CodexCursor
	b, cursorErr := os.ReadFile(path)
	if cursorErr == nil && json.Unmarshal(b, &cursor) != nil {
		e.Logf("invalid funding cursor; replaying rollout")
		cursor = CodexCursor{}
	}
	// A session's first capture is when the directory is swept, as for reply cursors:
	// nothing else ever removed a finished session's cursor.
	if os.IsNotExist(cursorErr) {
		defer hookrun.PruneState(filepath.Dir(path), e.Time().Add(-spool.MaxAge))
	}
	// Resolve the funding owner once (not per evidence): Codex's ChatGPT account, and only when the
	// session is on the subscription route. Empty on the API-key route or when unreadable — never a
	// stale or guessed id.
	accountID, _ := CodexOAuthAccountID()
	// The per-user identity behind a shared Team workspace account_id: Codex's account_id cannot tell
	// members apart, so also resolve the signed-in email and the stable opaque user_id (subscription
	// route only). account_email is emitted raw (the platform's terma-cli identity convention, renamed
	// to user.email downstream); account_user_id is the durable per-user principal the native OTel wire
	// never exposes.
	userEmail, userID, _ := CodexOAuthUser()
	// Keep capture below Codex Stop's three-second timeout.
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, readErr := ReadCodexFunding(ctx, rollout, in.TranscriptPath, cursor, func(ev harness.FundingEvidence) error {
		attrs := hookrun.AgentAttrs(ev.Attrs, in.AgentID, in.AgentType)
		attrs[hookrun.AttrTool], attrs[hookrun.AttrVersion] = codexTool, e.Version
		attrs[hookrun.AttrEvidenceSource], attrs[hookrun.AttrEvidenceStatus] = ev.Source, ev.Status
		attrs[hookrun.AttrSchemaVersion] = 1
		// Stamp the account only onto a real, present ChatGPT quota snapshot. A record with no OpenAI
		// rate-limit evidence (ev.Status != "present": rate_limits:null, or a session run through
		// --oss/--local-provider/a custom model_provider that emits none) is not this ChatGPT account's
		// usage, so it must not carry the id.
		if accountID != "" && ev.Status == hookrun.StatusPresent {
			attrs[hookrun.AttrAccountID] = accountID
		}
		// Same gate as account_id: the per-user identity belongs only on a real ChatGPT quota snapshot.
		if userEmail != "" && ev.Status == hookrun.StatusPresent {
			attrs["account_email"] = userEmail
		}
		if userID != "" && ev.Status == hookrun.StatusPresent {
			attrs["account_user_id"] = userID
		}
		attrs[hookrun.AttrProjectID] = r.ProjectID
		r.StampWorktree(attrs)
		if !ev.SourceTime.IsZero() {
			attrs["source_time"] = ev.SourceTime.UTC().Format(time.RFC3339Nano)
		}
		return e.Spool.Append(spool.Event{Time: e.Time(), Name: hookrun.EventSessionQuota, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	})
	if next != cursor {
		b, _ := json.Marshal(next)
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("funding cursor: %v", err)
		}
	}
	if readErr != nil {
		e.Logf("funding capture: %v", readErr)
	}
	// Capture progress separately: backlog is not a new unavailable quota.
	if status != "caught_up" && status != "append_failed" {
		name := hookrun.EventSessionCapture
		if status == "not_ready" {
			name = hookrun.EventSessionQuota
		}
		e.CaptureFunding(r, in.SessionID, codexTool, name, harness.FundingEvidence{Source: sourceCodexRollout, Status: status, Attrs: hookrun.AgentAttrs(map[string]any{"source_offset": next.Offset}, in.AgentID, in.AgentType)})
	}
}
