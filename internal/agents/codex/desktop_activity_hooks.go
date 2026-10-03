package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// captureCodexDesktopActivity fills the two gaps in repository hooks: model usage and
// hosted Extension actions.
func captureCodexDesktopActivity(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) {
		return
	}
	if !codexDesktopRoute(e, r) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, codexDesktopCursorDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hookrun.EvidenceID(codexRolloutID(in))+".json")
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor desktopCursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cursor)
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := readDesktopActivity(ctx, codexRolloutID(in), in.TranscriptPath, cursor, func(a desktopActivity) error {
		attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexRollout, "desktop")
		attrs["capture_surface"] = codexDesktopSurface
		hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, a.TurnID)
		hookrun.BoundedAttr(attrs, hookrun.AttrModel, a.Model)
		hookrun.BoundedAttr(attrs, "trace_id", a.TraceID)
		at := a.At
		if at.IsZero() || at.After(e.Time()) {
			at = e.Time()
		}
		ev := spool.Event{Time: at, SessionID: in.SessionID, Repo: r.Name, Repository: r.Repository, Attrs: attrs}
		switch a.Kind {
		case "model":
			ev.Name = hookrun.EventModelCall
			attrs["response_id"] = a.ID
			attrs["reported_input_tokens"] = a.InputTokens
			attrs["reported_cache_read_tokens"] = a.CachedInputTokens
			attrs["reported_cache_write_tokens"] = a.CacheWriteInputTokens
			attrs["reported_output_tokens"] = a.OutputTokens
			attrs["reasoning_output_tokens"] = a.ReasoningOutputTokens
		case "tool":
			ev.Name = hookrun.EventToolCall
			attrs[hookrun.AttrToolCallID] = a.ID
			attrs[hookrun.AttrToolName] = a.ToolName
			attrs[hookrun.AttrStatus] = "completed"
			if a.HasDuration {
				attrs["duration_ms"] = a.DurationMs
				attrs["duration_source"] = "rollout_item"
			}
			attrs["arguments"] = boundedCodexContent(a.Input)
		case "compaction":
			ev.Name = hookrun.EventCompaction
			attrs["item_id"] = a.ID
			if a.HasDuration {
				attrs["duration_ms"] = a.DurationMs
			}
			if !a.StartedAt.IsZero() {
				ev.Time = a.StartedAt
			}
		case "turn":
			ev.Name = hookrun.EventTurnSummary
			attrs[hookrun.AttrStatus] = a.Status
			hookrun.BoundedAttr(attrs, hookrun.AttrReason, a.Reason)
			if a.HasDuration {
				attrs["duration_ms"] = a.DurationMs
			}
			if a.HasTTFT {
				attrs["ttft_ms"] = a.TTFTMs
			}
			if !a.StartedAt.IsZero() {
				ev.Time = a.StartedAt
			}
		default:
			return nil
		}
		// Spooled without Env.EmitFor, so the binding is stamped here: with no project id the
		// flush drops it as unroutable.
		if r.ProjectID != "" {
			attrs[hookrun.AttrProjectID] = r.ProjectID
		}
		r.StampWorktree(attrs)
		return e.Spool.Append(ev)
	})
	if err != nil {
		e.Logf("desktop activity (%s): %v", status, err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			_ = hookrun.WriteState(path, b)
		}
	}
}
