package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// captureCodexDesktopActivity fills the two gaps in repository hooks: model usage
// and hosted Extension actions. Local tool calls are emitted by PostToolUse.
func (e Env) captureCodexDesktopActivity(ctx context.Context, r *repo, in *codexHookInput) {
	if e.Spool == nil || !session.ValidID(in.SessionID) {
		return
	}
	route, enabled := codexDesktopRoute(r)
	if !enabled {
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
	path := filepath.Join(dir, evidenceID(codexRolloutID(in))+".json")
	unlock, err := lockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var cursor harness.CodexDesktopCursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cursor)
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := harness.ReadCodexDesktopActivity(ctx, codexRolloutID(in), in.TranscriptPath, cursor, func(a harness.CodexDesktopActivity) error {
		attrs := evidenceAttrs(codexTool, sourceCodexRollout, "desktop")
		attrs["capture_surface"] = codexDesktopSurface
		boundedAttr(attrs, attrTurnID, a.TurnID)
		boundedAttr(attrs, attrModel, a.Model)
		boundedAttr(attrs, "trace_id", a.TraceID)
		at := a.At
		if at.IsZero() || at.After(e.now()) {
			at = e.now()
		}
		ev := spool.Event{Time: at, SessionID: in.SessionID, Repo: repoName(r.root), Attrs: attrs}
		switch a.Kind {
		case "model":
			ev.Name = EventModelCall
			attrs["response_id"] = a.ID
			attrs["reported_input_tokens"] = a.InputTokens
			attrs["reported_cache_read_tokens"] = a.CachedInputTokens
			attrs["reported_cache_write_tokens"] = a.CacheWriteInputTokens
			attrs["reported_output_tokens"] = a.OutputTokens
			attrs["reasoning_output_tokens"] = a.ReasoningOutputTokens
		case "tool":
			ev.Name = EventToolCall
			attrs[attrToolCallID] = a.ID
			attrs[attrToolName] = a.ToolName
			attrs[attrStatus] = "completed"
			if a.HasDuration {
				attrs["duration_ms"] = a.DurationMs
				attrs["duration_source"] = "rollout_item"
			}
			if route.IncludeToolContent {
				attrs["arguments"] = boundedCodexContent(a.Input)
			}
		case "compaction":
			ev.Name = EventCompaction
			attrs["item_id"] = a.ID
			if a.HasDuration {
				attrs["duration_ms"] = a.DurationMs
			}
			if !a.StartedAt.IsZero() {
				ev.Time = a.StartedAt
			}
		case "turn":
			ev.Name = EventTurnSummary
			attrs[attrStatus] = a.Status
			boundedAttr(attrs, attrReason, a.Reason)
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
		return e.Spool.Append(ev)
	})
	if err != nil {
		e.logf("desktop activity (%s): %v", status, err)
	}
	if next != cursor {
		if b, err := json.Marshal(next); err == nil {
			_ = writeState(path, b)
		}
	}
}
