package hookrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func (e Env) captureCodexFunding(ctx context.Context, r *repo, in *codexHookInput) {
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
	path := filepath.Join(dir, codexFundingCursorDir, evidenceID(rollout)+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	unlock, err := lockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor harness.CodexCursor
	b, cursorErr := os.ReadFile(path)
	if cursorErr == nil && json.Unmarshal(b, &cursor) != nil {
		e.logf("invalid funding cursor; replaying rollout")
		cursor = harness.CodexCursor{}
	}
	// A session's first capture is when the directory is swept, as for reply cursors:
	// nothing else ever removed a finished session's cursor.
	if os.IsNotExist(cursorErr) {
		defer pruneQuotaState(filepath.Dir(path), e.now().Add(-spool.MaxAge))
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
		attrs := agentAttrs(ev.Attrs, in.AgentID, in.AgentType)
		attrs[attrTool], attrs[attrVersion] = codexTool, e.Version
		attrs[attrEvidenceSource], attrs[attrEvidenceStatus] = ev.Source, ev.Status
		attrs[attrSchemaVersion] = 1
		// Stamp the account only onto a real, present ChatGPT quota snapshot. A record with no OpenAI
		// rate-limit evidence (ev.Status != "present": rate_limits:null, or a session run through
		// --oss/--local-provider/a custom model_provider that emits none) is not this ChatGPT account's
		// usage, so it must not carry the id.
		if accountID != "" && ev.Status == statusPresent {
			attrs[attrAccountID] = accountID
		}
		// Same gate as account_id: the per-user identity belongs only on a real ChatGPT quota snapshot.
		if userEmail != "" && ev.Status == statusPresent {
			attrs["account_email"] = userEmail
		}
		if userID != "" && ev.Status == statusPresent {
			attrs["account_user_id"] = userID
		}
		attrs[AttrProjectID] = r.projectID
		r.stampWorktree(attrs)
		if !ev.SourceTime.IsZero() {
			attrs["source_time"] = ev.SourceTime.UTC().Format(time.RFC3339Nano)
		}
		return e.Spool.Append(spool.Event{Time: e.now(), Name: EventSessionQuota, SessionID: in.SessionID, Repo: r.name, Attrs: attrs})
	})
	if next != cursor {
		b, _ := json.Marshal(next)
		if err := writeState(path, b); err != nil {
			e.logf("funding cursor: %v", err)
		}
	}
	if readErr != nil {
		e.logf("funding capture: %v", readErr)
	}
	// Capture progress separately: backlog is not a new unavailable quota.
	if status != "caught_up" && status != "append_failed" {
		name := EventSessionCapture
		if status == "not_ready" {
			name = EventSessionQuota
		}
		e.captureFunding(r, in.SessionID, codexTool, name, harness.FundingEvidence{Source: sourceCodexRollout, Status: status, Attrs: agentAttrs(map[string]any{"source_offset": next.Offset}, in.AgentID, in.AgentType)})
	}
}

func evidenceID(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
