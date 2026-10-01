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
	// Inside a subagent the cursor follows the child's rollout; evidence stays under the
	// session, with the agent named.
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
	// A session's first capture sweeps the directory: nothing else removes a finished cursor.
	if os.IsNotExist(cursorErr) {
		defer hookrun.PruneState(filepath.Dir(path), e.Time().Add(-spool.MaxAge))
	}
	// Resolved once: empty on the API-key route or when unreadable, never a guessed id.
	accountID, _ := CodexOAuthAccountID()
	// A Team workspace's account_id cannot tell members apart, so the email and the durable
	// user_id, which the native export never carries, travel too.
	userEmail, userID, _ := CodexOAuthUser()
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, readErr := ReadCodexFunding(ctx, rollout, in.TranscriptPath, cursor, func(ev harness.FundingEvidence) error {
		attrs := hookrun.AgentAttrs(ev.Attrs, in.AgentID, in.AgentType)
		attrs[hookrun.AttrTool], attrs[hookrun.AttrVersion] = codexTool, e.Version
		attrs[hookrun.AttrEvidenceSource], attrs[hookrun.AttrEvidenceStatus] = ev.Source, ev.Status
		attrs[hookrun.AttrSchemaVersion] = 1
		// Only a present ChatGPT quota snapshot carries the account: without rate-limit evidence
		// (a local or custom provider) it is not this account's usage.
		if accountID != "" && ev.Status == hookrun.StatusPresent {
			attrs[hookrun.AttrAccountID] = accountID
		}
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
