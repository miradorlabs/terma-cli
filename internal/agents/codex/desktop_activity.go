package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// desktopActivity is a hosted action or compaction from the rollout: the activity Codex's
// own telemetry does not report, so none of it counts twice.
type desktopActivity struct {
	Kind, ID, TurnID, TraceID, Model, ToolName, Input string
	DurationMs                                        int64
	HasDuration                                       bool
	At, StartedAt                                     time.Time
}

// desktopCursor stores only position and turn metadata, never content.
type desktopCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor"`
	TurnID   string `json:"turn_id,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
	Model    string `json:"model,omitempty"`
	Skipping bool   `json:"skipping,omitempty"`
	turnState
}

// readDesktopActivity reads at most 1 MiB and 128 relevant records per hook; the
// caller appends each activity before persisting the cursor.
func readDesktopActivity(ctx context.Context, sessionID, transcript string, cursor desktopCursor, admits func(cwd string) bool, emit func(desktopActivity) error) (desktopCursor, string, error) {
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
		cursor = desktopCursor{Identity: identity}
	}
	cursor.recheck(admits)
	old := cursor
	checkpoint := func(s string, e error) (desktopCursor, string, error) {
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
		} else if a, ok := codexDesktopActivityFrom(line, &cursor, sessionID, admits); ok && cursor.Admitted {
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

func codexDesktopActivityFrom(line []byte, cursor *desktopCursor, sessionID string, admits func(string) bool) (desktopActivity, bool) {
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			Type          string `json:"type"`
			TurnID        string `json:"turn_id"`
			Model         string `json:"model"`
			TraceID       string `json:"trace_id"`
			StartedAtMs   int64  `json:"started_at_ms"`
			CompletedAtMs int64  `json:"completed_at_ms"`
			Item          struct {
				Type  string `json:"type"`
				Kind  string `json:"kind"`
				ID    string `json:"id"`
				Query string `json:"query"`
			} `json:"item"`
			turnFields
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return desktopActivity{}, false
	}
	cursor.track(admits, rec.Type, rec.Payload.Type, rec.Payload.turnFields)
	if rec.Type == "turn_context" {
		if hookrun.EvidenceLabel.MatchString(rec.Payload.TurnID) {
			if cursor.TurnID != rec.Payload.TurnID {
				cursor.TraceID = ""
			}
			cursor.TurnID = rec.Payload.TurnID
		}
		if hookrun.EvidenceLabel.MatchString(rec.Payload.Model) {
			cursor.Model = rec.Payload.Model
		}
		return desktopActivity{}, false
	}
	if rec.Payload.TurnID != "" && hookrun.EvidenceLabel.MatchString(rec.Payload.TurnID) {
		if cursor.TurnID != rec.Payload.TurnID {
			cursor.TraceID = ""
		}
		cursor.TurnID = rec.Payload.TurnID
	}
	if rec.Type == "event_msg" && rec.Payload.Type == "task_started" {
		if hookrun.EvidenceLabel.MatchString(rec.Payload.TraceID) {
			cursor.TraceID = rec.Payload.TraceID
		}
		return desktopActivity{}, false
	}
	a := desktopActivity{TurnID: cursor.TurnID, TraceID: cursor.TraceID, Model: cursor.Model, At: rec.Timestamp}
	switch {
	case rec.Type == "event_msg" && rec.Payload.Type == "item_completed" && rec.Payload.Item.Type == "Extension":
		a.Kind, a.ID = "tool", rec.Payload.Item.ID
		a.ToolName, a.Input = "Extension:"+rec.Payload.Item.Kind, rec.Payload.Item.Query
		a.StartedAt, a.DurationMs, a.HasDuration = codexItemTiming(rec.Payload.StartedAtMs, rec.Payload.CompletedAtMs)
	case rec.Type == "event_msg" && rec.Payload.Type == "item_completed" && rec.Payload.Item.Type == "ContextCompaction":
		a.Kind, a.ID = "compaction", rec.Payload.Item.ID
		a.StartedAt, a.DurationMs, a.HasDuration = codexItemTiming(rec.Payload.StartedAtMs, rec.Payload.CompletedAtMs)
	default:
		return desktopActivity{}, false
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
