package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func captureCodexFunding(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) {
		return
	}
	// Inside a subagent the cursor follows the child's rollout; evidence stays under the
	// session, with the agent named.
	rollout := codexRolloutID(in)
	path := filepath.Join(e.StateDir, codexFundingCursorDir, hookrun.EvidenceID(rollout)+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor quotaCursor
	b, cursorErr := os.ReadFile(path)
	if cursorErr == nil && json.Unmarshal(b, &cursor) != nil {
		e.Logf("invalid funding cursor; replaying rollout")
		cursor = quotaCursor{}
	}
	// A session's first capture sweeps the directory: nothing else removes a finished cursor.
	if os.IsNotExist(cursorErr) {
		defer hookrun.PruneState(filepath.Dir(path), e.Time().Add(-spool.MaxAge))
	}
	// Resolved once: empty on the API-key route or when unreadable, never a guessed id.
	accountID, _ := oauthAccountID()
	// A Team workspace's account_id cannot tell members apart, so the email, which the native
	// export never carries, travels too, and the seat id the relay sends for a withheld one.
	userEmail := oauthEmail()
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, readErr := readFunding(ctx, rollout, in.TranscriptPath, cursor, turnAdmission(ctx, e), func(ev hookrun.FundingEvidence) error {
		attrs := hookrun.AgentAttrs(ev.Attrs, in.AgentID, in.AgentType)
		attrs[semconv.GenAIMainAgentNameKey] = codexTool
		attrs[semconv.TermaEvidenceSourceKey], attrs[semconv.TermaEvidenceStatusKey] = ev.Source, ev.Status
		// Only a present ChatGPT quota snapshot carries the account: without rate-limit evidence
		// (a local or custom provider) it is not this account's usage.
		if ev.Status == hookrun.StatusPresent {
			if accountID != "" {
				attrs[semconv.TermaAccountIDKey] = accountID
			}
			if userEmail != "" {
				// Delivery withholds the email with content; the seat id stays in its place.
				attrs[semconv.UserEmailKey] = userEmail
				if accountID != "" {
					attrs[semconv.TermaAccountSeatIDKey] = shape.SeatID(accountID, userEmail)
				}
			}
		}
		attrs[hookrun.AttrProjectID] = r.ProjectID
		if !ev.SourceTime.IsZero() {
			attrs[semconv.TermaObservationTimeKey] = ev.SourceTime.UTC().Format(time.RFC3339Nano)
		}
		return e.Spool.Append(spool.Event{Time: e.Time(), Name: semconv.TermaSessionQuotaEvent, SessionID: in.SessionID, Repository: r.Repository, Attrs: attrs})
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
		name := semconv.TermaSessionCaptureEvent
		if status == "not_ready" {
			name = semconv.TermaSessionQuotaEvent
		}
		e.CaptureFunding(r, in.SessionID, codexTool, name, hookrun.FundingEvidence{Source: sourceCodexRollout, Status: status, Attrs: hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType)})
	}
}
