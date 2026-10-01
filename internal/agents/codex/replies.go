package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Codex exports prompts and tool content but never its replies, which exist only in the
// rollout. This is the one place terma reads conversation content, and only under
// CodexRepliesConsented: both or neither of prompt and reply travel.

// CodexReply is one assistant message from a rollout.
type CodexReply struct {
	// ID is Codex's own message id (`msg_…`), else derived from the record's position.
	ID string
	// Text is cut to the caller's limit on a rune boundary; Bytes is the full length.
	Text      string
	Bytes     int
	Truncated bool
	// Phase is "commentary" between tool calls, "final_answer" for the reply, or "".
	Phase string
	// TraceID is the turn's OTel trace id, which the platform calls a turn; TurnID is the
	// rollout's own.
	TurnID  string
	TraceID string
	// At is when Codex recorded the message, not when the hook read it.
	At time.Time
}

// CodexReplyCursor holds no transcript text: an offset, an anchor that detects a rewritten
// file, and the current turn, which spans hook invocations.
type CodexReplyCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor"`
	TurnID   string `json:"turn_id,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
	Skipping bool   `json:"skipping,omitempty"`
}

// codexReplyBatch bounds one invocation inside Stop's three seconds; the rest is
// "backlog" for the next turn's hook.
const codexReplyBatch = 32

// ReadCodexReplies emits the messages since cursor, oldest first, and the cursor to persist
// once they are spooled; a crash in between replays the same ids. Status is "caught_up",
// "backlog", "incomplete" (mid-record), or why the rollout could not be opened.
func ReadCodexReplies(ctx context.Context, sessionID, transcript string, cursor CodexReplyCursor, maxText int, emit func(CodexReply) error) (CodexReplyCursor, string, error) {
	f, status := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return cursor, status, nil
	}
	defer func() { _ = f.Close() }()
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
		cursor = CodexReplyCursor{Identity: identity}
	}
	old := cursor
	checkpoint := func(status string, err error) (CodexReplyCursor, string, error) {
		cursor.Anchor = cursorAnchor(f, cursor.Offset)
		return cursor, status, err
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
		if emitted >= codexReplyBatch {
			return checkpoint("backlog", nil)
		}
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if !complete {
			if cursor.Skipping {
				cursor.Offset += int64(len(data))
				return checkpoint("backlog", nil)
			}
			// The end of a read chunk is not an oversized record: reread from this line's start.
			if len(data) < codexTailLimit {
				if cursor.Offset+int64(len(data)) < st.Size() {
					return checkpoint("backlog", nil)
				}
				return checkpoint("incomplete", nil)
			}
			// A record over the budget is a tool's output, not a reply: stepped over, turn unknown.
			cursor.Offset += int64(len(data))
			cursor.Skipping, cursor.TurnID, cursor.TraceID = true, "", ""
			return checkpoint("backlog", nil)
		}
		if cursor.Skipping {
			cursor.Skipping = false
		} else if reply, ok := codexReplyFrom(line, &cursor, sessionID, maxText); ok {
			if err := emit(reply); err != nil {
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
	if cursor.Skipping {
		return checkpoint("incomplete", nil)
	}
	return checkpoint("caught_up", nil)
}

// codexReplyFrom keeps the cursor's turn current and returns a non-empty assistant
// message; an unparseable record is left alone.
func codexReplyFrom(line []byte, cursor *CodexReplyCursor, sessionID string, maxText int) (CodexReply, bool) {
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			ID      string `json:"id"`
			Phase   string `json:"phase"`
			TurnID  string `json:"turn_id"`
			TraceID string `json:"trace_id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return CodexReply{}, false
	}
	switch {
	case rec.Type == "turn_context", rec.Type == "event_msg" && (rec.Payload.Type == "task_started" || rec.Payload.Type == "turn_started"):
		// turn_context repeats the turn id without its trace id; only a different turn ends this one.
		if rec.Payload.TurnID != cursor.TurnID {
			cursor.TurnID, cursor.TraceID = "", ""
			if hookrun.EvidenceLabel.MatchString(rec.Payload.TurnID) {
				cursor.TurnID = rec.Payload.TurnID
			}
		}
		if codexTraceID(rec.Payload.TraceID) {
			cursor.TraceID = rec.Payload.TraceID
		}
		return CodexReply{}, false
	case rec.Type == "event_msg" && (rec.Payload.Type == "task_complete" || rec.Payload.Type == "turn_complete" || rec.Payload.Type == "turn_aborted"):
		cursor.TurnID, cursor.TraceID = "", ""
		return CodexReply{}, false
	case rec.Type != "response_item" || rec.Payload.Type != "message" || rec.Payload.Role != "assistant":
		return CodexReply{}, false
	}
	var text strings.Builder
	for _, part := range rec.Payload.Content {
		if part.Type == "output_text" {
			text.WriteString(part.Text)
		}
	}
	full := text.String()
	if strings.TrimSpace(full) == "" {
		return CodexReply{}, false
	}
	reply := CodexReply{
		ID: rec.Payload.ID, Bytes: len(full), Text: full,
		TurnID: cursor.TurnID, TraceID: cursor.TraceID, At: rec.Timestamp,
	}
	if !hookrun.EvidenceLabel.MatchString(reply.ID) {
		reply.ID = fundingHash(sessionID + cursor.Identity + fmt.Sprint(cursor.Offset))
	}
	if hookrun.EvidenceLabel.MatchString(rec.Payload.Phase) {
		reply.Phase = rec.Payload.Phase
	}
	if maxText > 0 && len(full) > maxText {
		cut := maxText
		for cut > 0 && !utf8.RuneStart(full[cut]) {
			cut--
		}
		reply.Text, reply.Truncated = full[:cut], true
	}
	return reply, true
}

// codexTraceID admits 32 lowercase hex digits, not the all-zero "no trace" id.
func codexTraceID(s string) bool {
	if len(s) != 32 || s == strings.Repeat("0", 32) {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
