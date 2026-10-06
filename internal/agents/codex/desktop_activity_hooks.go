package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// captureCodexDesktopActivity sends the rollout's hosted Extension actions and compactions.
// Codex's own telemetry reports its model calls, turns, prompts and tool calls through the
// relay on every surface, so terma sending them too counted each twice (#95). It returns
// the cursor it reached, which names the rollout's current turn.
func captureCodexDesktopActivity(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *codexHookInput) desktopCursor {
	if e.Spool == nil || !session.ValidID(in.SessionID) {
		return desktopCursor{}
	}
	if !codexDesktopRoute(e, r) {
		return desktopCursor{}
	}
	dir := filepath.Join(e.StateDir, codexDesktopCursorDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return desktopCursor{}
	}
	path := filepath.Join(dir, hookrun.EvidenceID(codexRolloutID(in))+".json")
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return desktopCursor{}
	}
	defer unlock()
	var cursor desktopCursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cursor)
	}
	ctx, cancel := context.WithTimeout(ctx, codexCaptureTimeout)
	defer cancel()
	next, status, err := readDesktopActivity(ctx, codexRolloutID(in), in.TranscriptPath, cursor, turnAdmission(ctx, e), func(a desktopActivity) error {
		attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexRollout, "desktop")
		attrs[semconv.TermaCaptureSurfaceKey] = semconv.TermaCaptureSurfaceDesktop
		hookrun.BoundedAttr(attrs, semconv.TermaTurnIDKey, a.TurnID)
		hookrun.BoundedAttr(attrs, semconv.GenAIRequestModelKey, a.Model)
		at := a.At
		if at.IsZero() || at.After(e.Time()) {
			at = e.Time()
		}
		ev := spool.Event{Time: at, SessionID: in.SessionID, TraceID: a.TraceID, Repository: r.Repository, Attrs: attrs}
		switch a.Kind {
		case "tool":
			ev.Name = semconv.TermaToolCallEvent
			attrs[semconv.GenAIToolCallIDKey] = a.ID
			attrs[semconv.GenAIToolNameKey] = a.ToolName
			attrs[semconv.GenAIToolTypeKey] = codexHostedToolType
			attrs[semconv.TermaOperationStatusKey] = "completed"
			if a.HasDuration {
				attrs[semconv.TermaOperationDurationMsKey] = a.DurationMs
			}
			attrs[semconv.GenAIToolCallArgumentsKey] = codexToolArguments(a.Input)
		case "compaction":
			ev.Name = semconv.TermaCompactionEvent
			attrs[semconv.GenAIConversationCompactedKey] = true
			attrs[semconv.TermaCompactionItemIDKey] = a.ID
			if a.HasDuration {
				attrs[semconv.TermaOperationDurationMsKey] = a.DurationMs
			}
			if !a.StartedAt.IsZero() {
				ev.Time = a.StartedAt
			}
		default:
			return nil
		}
		// Spooled without Env.EmitFor, so the team is stamped here: with no project id the
		// flush drops it as unroutable.
		if r.ProjectID != "" {
			attrs[hookrun.AttrProjectID] = r.ProjectID
		}
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
	return next
}

// codexHostedToolType is gen_ai.tool.type for a tool Codex ran itself.
const codexHostedToolType = "extension"

// codexToolArguments is a hosted call's input as gen_ai.tool.call.arguments: a JSON object
// or array as structured data, anything else, a cut one included, as text.
func codexToolArguments(input string) any {
	bounded := boundedCodexContent(input)
	var v any
	if len(bounded) == len(input) && json.Unmarshal([]byte(input), &v) == nil {
		switch v.(type) {
		case map[string]any, []any:
			return v
		}
	}
	return bounded
}
