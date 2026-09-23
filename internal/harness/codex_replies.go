package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Codex exports what the developer said and what its tools did, and never what it
// answered: its OTel events carry `prompt`, tool `arguments` and tool `output`, and a
// `response.completed` that is token counts and nothing else. There is no event for the
// model's reply and no switch that adds one (checked against every record of a live
// 0.155.1 session, 2026-09-19). So a Codex session on the platform reads as a person
// talking to tools — the replies exist only in the rollout, which this reads.
//
// It is the one place terma reads conversation content, and it does so only for a
// developer whose Codex already exports their prompts (see hookrun.codexRepliesConsented):
// the reply to a prompt is no more private than the prompt, and both or neither travel.

// CodexReply is one assistant message from a rollout.
type CodexReply struct {
	// ID is the replay identity: Codex's own message id (`msg_…`) when the record has
	// one, else derived from where the record sits in the file.
	ID string
	// Text is the message, cut to the caller's limit on a rune boundary. Bytes is its
	// full length and Truncated says whether the two differ.
	Text      string
	Bytes     int
	Truncated bool
	// Phase is Codex's own label for the message — "commentary" for what it says between
	// tool calls, "final_answer" for the turn's reply — or "" when the record has none.
	Phase string
	// TurnID is the rollout's turn id. TraceID is the turn's OTel trace id, which is what
	// the platform's Codex adapter calls a turn: the id every natively exported event of
	// the turn carries. Without it the message is still in the session, but in no turn.
	TurnID  string
	TraceID string
	// At is when Codex recorded the message, which is when it was said — not when the
	// Stop hook got round to reading it.
	At time.Time
}

// CodexReplyCursor is where a session's reply capture has got to. Like CodexCursor it
// holds no transcript text: an offset past the acknowledged records, an anchor that
// detects a truncated or rewritten file, and the turn the next record belongs to, which
// has to survive between hook invocations because a turn's records span several.
type CodexReplyCursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor"`
	TurnID   string `json:"turn_id,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
	Skipping bool   `json:"skipping,omitempty"`
}

// codexReplyBatch bounds one invocation: the Stop hook has three seconds for everything
// it does, and a session captured for the first time half-way through has a history to
// catch up on. What is left is "backlog", read by the next turn's hook.
const codexReplyBatch = 32

// ReadCodexReplies emits the assistant messages recorded since cursor, oldest first, and
// returns the cursor to persist once they are spooled. The caller spools before it
// acknowledges; a crash in between replays the same ids. maxText bounds each message.
//
// Status is "caught_up", "backlog" (more to read: the batch or the byte budget ran out),
// "incomplete" (the file ends mid-record — Codex is still writing), or the reason the
// rollout could not be opened. A rollout that was replaced or truncated is read again
// from the start: the ids are Codex's own, so what was already sent is sent as itself.
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
			// The end of a read chunk is not an oversized record: come back to this
			// line's start with the whole budget.
			if len(data) < codexTailLimit {
				if cursor.Offset+int64(len(data)) < st.Size() {
					return checkpoint("backlog", nil)
				}
				return checkpoint("incomplete", nil)
			}
			// A record larger than the budget — a tool's output, not a reply — is
			// stepped over; what it says about the turn is unknown, so the turn is too.
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

// codexReplyFrom reads one rollout record: it keeps the cursor's turn current, and returns
// the record as a reply when it is an assistant message with something in it. A record
// that does not parse says nothing about the turn either way and is left alone.
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
		// turn_context repeats the turn's id without its trace id; only a record that
		// names a different turn ends the one the cursor is in.
		if rec.Payload.TurnID != cursor.TurnID {
			cursor.TurnID, cursor.TraceID = "", ""
			if evidenceLabel.MatchString(rec.Payload.TurnID) {
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
	if !evidenceLabel.MatchString(reply.ID) {
		reply.ID = fundingHash(sessionID + cursor.Identity + fmt.Sprint(cursor.Offset))
	}
	if evidenceLabel.MatchString(rec.Payload.Phase) {
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

// codexTraceID admits an OTel trace id as Codex writes it: 32 lowercase hex digits, and
// not the all-zero id that means "no trace".
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
