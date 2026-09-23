package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// CodexCursor contains no transcript text. Offset points past acknowledged records;
// Anchor detects truncation/rewrite, including regrowth beyond the previous offset.
// TurnID is carried across bounded reads, never inferred from the capturing hook.
type CodexCursor struct {
	SeenQuota bool   `json:"seen_quota,omitempty"`
	Identity  string `json:"identity"`
	Stream    string `json:"stream"`
	Offset    int64  `json:"offset"`
	Anchor    string `json:"anchor"`
	TurnID    string `json:"turn_id,omitempty"`
	Skipping  bool   `json:"skipping,omitempty"`
}

func fundingHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func cursorAnchor(f *os.File, offset int64) string {
	if offset <= 0 {
		return ""
	}
	b := make([]byte, min(offset, 64))
	n, err := f.ReadAt(b, offset-int64(len(b)))
	if err != nil || n != len(b) {
		return ""
	}
	return fundingHash(string(b))
}

// ReadCodexFunding emits each newly observed quota, including unchanged and null
// records. The caller must spool before acknowledging and persist the returned
// cursor. A crash between append and checkpoint replays stable observation IDs.
// At most 1 MiB and 256 evidence records are processed per invocation. backlog and
// incomplete mean retry later, not lost data; gap events identify skipped data.
func ReadCodexFunding(ctx context.Context, sessionID, transcript string, cursor CodexCursor, emit func(FundingEvidence) error) (CodexCursor, string, error) {
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
	reset := cursor.Identity != "" && (cursor.Identity != identity || cursor.Offset < 0 || cursor.Offset > st.Size() || (cursor.Offset > 0 && cursor.Anchor != cursorAnchor(f, cursor.Offset)))
	old := cursor
	if cursor.Identity == "" || reset {
		cursor = CodexCursor{Identity: identity, Stream: fundingHash(identity + old.Stream + fmt.Sprint(old.Offset))}
	}
	send := func(e FundingEvidence, offset int64) error {
		if e.Attrs == nil {
			e.Attrs = map[string]any{}
		}
		e.Attrs["source_offset"] = offset
		e.Attrs["source_stream"] = cursor.Stream
		e.Attrs["observation_id"] = fundingHash(sessionID + cursor.Stream + fmt.Sprint(offset) + e.Status)
		if cursor.TurnID != "" {
			e.Attrs["turn_id"] = cursor.TurnID
		}
		return emit(e)
	}
	gap := func(reason string, offset int64) error {
		return send(FundingEvidence{Source: "codex_rollout", Status: "gap", Attrs: map[string]any{"gap_reason": reason}}, offset)
	}
	if reset {
		if err := gap("rollout_replaced_or_truncated", 0); err != nil {
			return old, "append_failed", err
		}
	}
	checkpoint := func(status string, err error) (CodexCursor, string, error) {
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
		if emitted >= 256 {
			return checkpoint("backlog", nil)
		}
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if !complete {
			if cursor.Skipping {
				cursor.Offset += int64(len(data))
				return checkpoint("backlog", nil)
			}
			// Do not mistake the end of a read chunk for an oversized line. Retry
			// from this line's start with the full budget on the next invocation.
			if len(data) < codexTailLimit {
				if cursor.Offset+int64(len(data)) < st.Size() {
					return checkpoint("backlog", nil)
				}
				return checkpoint("incomplete", nil)
			}
			if err := gap("oversized_record", cursor.Offset); err != nil {
				return checkpoint("append_failed", err)
			}
			cursor.Offset += int64(len(data))
			cursor.Skipping = true
			cursor.TurnID = ""
			return checkpoint("backlog", nil)
		}
		if cursor.Skipping {
			cursor.Skipping = false
		} else {
			var rec struct {
				Timestamp time.Time `json:"timestamp"`
				Type      string    `json:"type"`
				Payload   struct {
					Type       string          `json:"type"`
					TurnID     string          `json:"turn_id"`
					RateLimits json.RawMessage `json:"rate_limits"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) != nil {
				if err := gap("malformed_record", cursor.Offset); err != nil {
					return checkpoint("append_failed", err)
				}
				emitted++
				cursor.TurnID = ""
			} else if rec.Type == "turn_context" || (rec.Type == "event_msg" && (rec.Payload.Type == "task_started" || rec.Payload.Type == "turn_started")) {
				cursor.TurnID = ""
				if evidenceLabel.MatchString(rec.Payload.TurnID) {
					cursor.TurnID = rec.Payload.TurnID
				}
			} else if rec.Type == "event_msg" {
				switch rec.Payload.Type {
				case "task_complete", "turn_complete", "turn_aborted":
					cursor.TurnID = ""
				case "token_count":
					if len(rec.Payload.RateLimits) > 0 {
						if err := send(codexQuota(rec.Payload.RateLimits, rec.Timestamp), cursor.Offset); err != nil {
							return checkpoint("append_failed", err)
						}
						cursor.SeenQuota = true
						emitted++
					}
				}
			}
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
	if !cursor.SeenQuota {
		return checkpoint("not_ready", nil)
	}
	return checkpoint("caught_up", nil)
}
