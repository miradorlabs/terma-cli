package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// CodexDesktopActivity is a completed response or hosted action from Codex's local
// rollout. Project hooks supply ordinary tool calls; only categories without a hook
// are read here, so one action cannot be counted twice.
type CodexDesktopActivity struct {
	Kind, ID, TurnID, TraceID, Model, ToolName, Input     string
	Status, Reason                                        string
	InputTokens, CachedInputTokens, CacheWriteInputTokens int64
	OutputTokens, ReasoningOutputTokens                   int64
	DurationMs, TTFTMs                                    int64
	HasDuration, HasTTFT                                  bool
	At, StartedAt                                         time.Time
}

// CodexDesktopCursor stores only position and turn metadata, never content.
type CodexDesktopCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor"`
	TurnID   string `json:"turn_id,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
	Model    string `json:"model,omitempty"`
	Skipping bool   `json:"skipping,omitempty"`
}

// ReadCodexDesktopActivity reads at most 1 MiB and 128 relevant records per hook.
// The caller must append each activity before persisting the returned cursor.
func ReadCodexDesktopActivity(ctx context.Context, sessionID, transcript string, cursor CodexDesktopCursor, emit func(CodexDesktopActivity) error) (CodexDesktopCursor, string, error) {
	f, status := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return cursor, status, nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cursor, "unreadable", err
	}
	head := make([]byte, min(st.Size(), 64<<10))
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return cursor, "unreadable", err
	}
	first, _, _ := bytes.Cut(head[:n], []byte{'\n'})
	identity := fundingHash(rolloutFileIdentity(st) + string(first))
	if cursor.Identity != identity || cursor.Offset < 0 || cursor.Offset > st.Size() ||
		(cursor.Offset > 0 && cursor.Anchor != cursorAnchor(f, cursor.Offset)) {
		cursor = CodexDesktopCursor{Identity: identity}
	}
	old := cursor
	checkpoint := func(s string, e error) (CodexDesktopCursor, string, error) {
		cursor.Anchor = cursorAnchor(f, cursor.Offset)
		return cursor, s, e
	}
	data := make([]byte, min(int64(codexTailLimit), st.Size()-cursor.Offset))
	n, err = f.ReadAt(data, cursor.Offset)
	if err != nil && err != io.EOF {
		return old, "unreadable", err
	}
	data = data[:n]
	emitted := 0
	for len(data) > 0 {
		if ctx.Err() != nil {
			return checkpoint("backlog", ctx.Err())
		}
		if emitted >= 128 {
			return checkpoint("backlog", nil)
		}
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if !complete {
			if cursor.Skipping {
				cursor.Offset += int64(len(data))
				return checkpoint("backlog", nil)
			}
			if len(data) < codexTailLimit {
				if cursor.Offset+int64(len(data)) < st.Size() {
					return checkpoint("backlog", nil)
				}
				return checkpoint("incomplete", nil)
			}
			cursor.Offset += int64(len(data))
			cursor.Skipping = true
			return checkpoint("backlog", nil)
		}
		if cursor.Skipping {
			cursor.Skipping = false
		} else if a, ok := codexDesktopActivityFrom(line, &cursor, sessionID); ok {
			if err := emit(a); err != nil {
				return checkpoint("append_failed", err)
			}
			emitted++
		}
		cursor.Offset += int64(len(line) + 1)
		data = rest
	}
	if cursor.Offset < st.Size() {
		return checkpoint("backlog", nil)
	}
	return checkpoint("caught_up", nil)
}

func codexDesktopActivityFrom(line []byte, cursor *CodexDesktopCursor, sessionID string) (CodexDesktopActivity, bool) {
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			Type          string `json:"type"`
			TurnID        string `json:"turn_id"`
			Model         string `json:"model"`
			ResponseID    string `json:"response_id"`
			TraceID       string `json:"trace_id"`
			Reason        string `json:"reason"`
			StartedAt     int64  `json:"started_at"`
			DurationMs    *int64 `json:"duration_ms"`
			TTFTMs        *int64 `json:"time_to_first_token_ms"`
			StartedAtMs   int64  `json:"started_at_ms"`
			CompletedAtMs int64  `json:"completed_at_ms"`
			Usage         struct {
				Input      int64 `json:"input_tokens"`
				Cached     int64 `json:"cached_input_tokens"`
				CacheWrite int64 `json:"cache_write_input_tokens"`
				Output     int64 `json:"output_tokens"`
				Reasoning  int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Item struct {
				Type  string `json:"type"`
				Kind  string `json:"kind"`
				ID    string `json:"id"`
				Query string `json:"query"`
			} `json:"item"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return CodexDesktopActivity{}, false
	}
	if rec.Type == "turn_context" {
		if evidenceLabel.MatchString(rec.Payload.TurnID) {
			if cursor.TurnID != rec.Payload.TurnID {
				cursor.TraceID = ""
			}
			cursor.TurnID = rec.Payload.TurnID
		}
		if evidenceLabel.MatchString(rec.Payload.Model) {
			cursor.Model = rec.Payload.Model
		}
		return CodexDesktopActivity{}, false
	}
	if rec.Payload.TurnID != "" && evidenceLabel.MatchString(rec.Payload.TurnID) {
		if cursor.TurnID != rec.Payload.TurnID {
			cursor.TraceID = ""
		}
		cursor.TurnID = rec.Payload.TurnID
	}
	if rec.Type == "event_msg" && rec.Payload.Type == "task_started" {
		if evidenceLabel.MatchString(rec.Payload.TraceID) {
			cursor.TraceID = rec.Payload.TraceID
		}
		return CodexDesktopActivity{}, false
	}
	a := CodexDesktopActivity{TurnID: cursor.TurnID, TraceID: cursor.TraceID, Model: cursor.Model, At: rec.Timestamp}
	switch {
	case rec.Type == "token_usage_record":
		a.Kind, a.ID = "model", rec.Payload.ResponseID
		a.InputTokens, a.CachedInputTokens, a.CacheWriteInputTokens = rec.Payload.Usage.Input, rec.Payload.Usage.Cached, rec.Payload.Usage.CacheWrite
		a.OutputTokens, a.ReasoningOutputTokens = rec.Payload.Usage.Output, rec.Payload.Usage.Reasoning
	case rec.Type == "event_msg" && rec.Payload.Type == "item_completed" && rec.Payload.Item.Type == "Extension":
		a.Kind, a.ID = "tool", rec.Payload.Item.ID
		a.ToolName, a.Input = "Extension:"+rec.Payload.Item.Kind, rec.Payload.Item.Query
		a.StartedAt, a.DurationMs, a.HasDuration = codexItemTiming(rec.Payload.StartedAtMs, rec.Payload.CompletedAtMs)
	case rec.Type == "event_msg" && rec.Payload.Type == "item_completed" && rec.Payload.Item.Type == "ContextCompaction":
		a.Kind, a.ID = "compaction", rec.Payload.Item.ID
		a.StartedAt, a.DurationMs, a.HasDuration = codexItemTiming(rec.Payload.StartedAtMs, rec.Payload.CompletedAtMs)
	case rec.Type == "event_msg" && (rec.Payload.Type == "task_complete" || rec.Payload.Type == "turn_aborted"):
		a.Kind, a.ID = "turn", cursor.TurnID
		if rec.Payload.Type == "turn_aborted" {
			a.Status, a.Reason = "cancelled", rec.Payload.Reason
		} else {
			a.Status = "completed"
		}
		if rec.Payload.StartedAt > 0 {
			a.StartedAt = time.Unix(rec.Payload.StartedAt, 0).UTC()
		}
		if rec.Payload.DurationMs != nil && *rec.Payload.DurationMs >= 0 {
			a.DurationMs, a.HasDuration = *rec.Payload.DurationMs, true
		}
		if rec.Payload.TTFTMs != nil && *rec.Payload.TTFTMs >= 0 {
			a.TTFTMs, a.HasTTFT = *rec.Payload.TTFTMs, true
		}
	default:
		return CodexDesktopActivity{}, false
	}
	if a.ID == "" {
		a.ID = fundingHash(sessionID + cursor.Identity + fmt.Sprint(cursor.Offset))
	}
	return a, true
}

func codexItemTiming(start, end int64) (time.Time, int64, bool) {
	if start <= 0 || end < start {
		return time.Time{}, 0, false
	}
	return time.UnixMilli(start).UTC(), end - start, true
}
