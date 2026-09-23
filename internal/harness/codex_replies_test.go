package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The record shapes below are Codex 0.155.1's, from a live rollout (2026-09-19): a turn
// opens with task_started carrying the rollout's turn_id *and* the turn's OTel trace_id,
// turn_context repeats the turn_id alone, and an assistant message is a response_item with
// Codex's own message id, a phase, and output_text parts.
const (
	replyTurnA  = "01a0bae4-4a41-7910-9022-15897b3021ae"
	replyTraceA = "ae2a5e6f8e0168ae5ec2efa2c8772b02"
	replyTurnB  = "01a0bae6-b17d-7550-a2bb-56b4068c59cb"
	replyTraceB = "c3c1b9b52b67c45776adda8c4001c1ae"
)

func replyTaskStarted(turn, trace string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":%q,"trace_id":%q}}`+"\n", turn, trace)
}
func replyTurnContext(turn string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-19T18:18:39.100Z","type":"turn_context","payload":{"turn_id":%q,"model":"gpt-5.6-luna"}}`+"\n", turn)
}
func replyMessage(role, id, phase, at string, parts ...string) string {
	var content []string
	for _, p := range parts {
		content = append(content, fmt.Sprintf(`{"type":"output_text","text":%q}`, p))
	}
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","ordinal":7,"payload":{"type":"message","role":%q,"id":%q,"phase":%q,"content":[%s]}}`+"\n",
		at, role, id, phase, strings.Join(content, ","))
}
func replyTaskComplete(turn string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-19T18:19:48.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":%q,"last_agent_message":"never read from here"}}`+"\n", turn)
}

func readReplies(t *testing.T, path string, c CodexReplyCursor, maxText int) (CodexReplyCursor, string, []CodexReply) {
	t.Helper()
	var out []CodexReply
	next, status, err := ReadCodexReplies(context.Background(), testCodexID, path, c, maxText, func(r CodexReply) error { out = append(out, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return next, status, out
}

// What Codex said, in order, each in the turn it was said in — and the turn is named by
// the trace id, because that is what every natively exported event of the turn carries.
func TestCodexRepliesCarryTheirTurnAndCodexsOwnIdentity(t *testing.T) {
	path := sequenceFixture(t,
		replyTaskStarted(replyTurnA, replyTraceA)+replyTurnContext(replyTurnA)+
			replyMessage("user", "msg_u1", "", "2026-09-19T18:18:39.200Z", "which ones can we remove")+
			replyMessage("developer", "msg_d1", "", "2026-09-19T18:18:39.210Z", "<permissions instructions>")+
			replyMessage("assistant", "msg_a1", "commentary", "2026-09-19T18:18:41.265Z", "I'll look at the ", "command list first.")+
			`{"timestamp":"2026-09-19T18:18:42.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}`+"\n"+
			replyMessage("assistant", "msg_a2", "final_answer", "2026-09-19T18:19:47.000Z", "Three of them can go.")+
			replyTaskComplete(replyTurnA)+
			replyTaskStarted(replyTurnB, replyTraceB)+
			replyMessage("assistant", "msg_a3", "final_answer", "2026-09-19T18:21:00.000Z", "Done."))
	cursor, status, replies := readReplies(t, path, CodexReplyCursor{}, 1<<10)
	if status != "caught_up" || len(replies) != 3 {
		t.Fatalf("status %s, replies %+v", status, replies)
	}
	want := []CodexReply{
		{ID: "msg_a1", Text: "I'll look at the command list first.", Phase: "commentary", TurnID: replyTurnA, TraceID: replyTraceA},
		{ID: "msg_a2", Text: "Three of them can go.", Phase: "final_answer", TurnID: replyTurnA, TraceID: replyTraceA},
		{ID: "msg_a3", Text: "Done.", Phase: "final_answer", TurnID: replyTurnB, TraceID: replyTraceB},
	}
	for i, w := range want {
		got := replies[i]
		if got.ID != w.ID || got.Text != w.Text || got.Phase != w.Phase || got.TurnID != w.TurnID || got.TraceID != w.TraceID || got.Truncated || got.Bytes != len(w.Text) {
			t.Errorf("reply %d = %+v, want %+v", i, got, w)
		}
	}
	// When it was said, not when it was read: a turn's replies are all read at its end.
	if !replies[0].At.Equal(time.Date(2026, 9, 19, 18, 18, 41, 265_000_000, time.UTC)) {
		t.Errorf("reply time = %v", replies[0].At)
	}
	// Nothing but an assistant's message is a reply: not the developer's prompt, not the
	// instructions Codex was given, not a tool call, not task_complete's summary of it.
	for _, r := range replies {
		if strings.Contains(r.Text, "remove") || strings.Contains(r.Text, "permissions") || strings.Contains(r.Text, "never read") {
			t.Errorf("captured something that is not a reply: %q", r.Text)
		}
	}
	// Acknowledged is acknowledged.
	if _, _, again := readReplies(t, path, cursor, 1<<10); len(again) != 0 {
		t.Fatalf("replayed acknowledged replies: %+v", again)
	}
}

// A turn's records span hook invocations: the turn is opened in one read and the reply
// arrives in the next, so the cursor carries the turn — both of its ids — across.
func TestCodexRepliesKeepTheTurnBetweenReads(t *testing.T) {
	half := replyMessage("assistant", "msg_a1", "commentary", "2026-09-19T18:18:41.265Z", "Working on it.")
	path := sequenceFixture(t, replyTaskStarted(replyTurnA, replyTraceA)+replyTurnContext(replyTurnA)+half[:len(half)/2])
	cursor, status, replies := readReplies(t, path, CodexReplyCursor{}, 1<<10)
	if status != "incomplete" || len(replies) != 0 || cursor.TurnID != replyTurnA || cursor.TraceID != replyTraceA {
		t.Fatalf("status %s replies %v cursor %+v — a half-written record is not read, and the turn is remembered", status, replies, cursor)
	}
	appendRollout(t, path, half[len(half)/2:])
	_, status, replies = readReplies(t, path, cursor, 1<<10)
	if status != "caught_up" || len(replies) != 1 || replies[0].TraceID != replyTraceA || replies[0].Text != "Working on it." {
		t.Fatalf("status %s replies %+v", status, replies)
	}
}

// A long reply is cut on a rune boundary and says that it was; an empty one is not a reply;
// a message with no id of Codex's own still gets an identity that a replay reproduces.
func TestCodexRepliesAreBoundedAndAlwaysIdentified(t *testing.T) {
	long := strings.Repeat("é", 40) // two bytes each
	path := sequenceFixture(t, replyTaskStarted(replyTurnA, replyTraceA)+
		replyMessage("assistant", "msg_long", "final_answer", "2026-09-19T18:18:41.000Z", long)+
		replyMessage("assistant", "msg_blank", "commentary", "2026-09-19T18:18:42.000Z", "  \n ")+
		replyMessage("assistant", "", "commentary", "2026-09-19T18:18:43.000Z", "no id of its own")+
		replyMessage("assistant", "not a safe label/../x", "commentary", "2026-09-19T18:18:44.000Z", "an id terma will not use"))
	_, _, replies := readReplies(t, path, CodexReplyCursor{}, 21)
	if len(replies) != 3 {
		t.Fatalf("replies: %+v", replies)
	}
	if r := replies[0]; !r.Truncated || r.Bytes != 80 || r.Text != strings.Repeat("é", 10) {
		t.Errorf("long reply = %q (truncated=%v bytes=%d); want 10 whole runes of 80 bytes", r.Text, r.Truncated, r.Bytes)
	}
	_, _, again := readReplies(t, path, CodexReplyCursor{}, 21)
	for i := 1; i < 3; i++ {
		if replies[i].ID == "" || len(replies[i].ID) != 64 || replies[i].ID != again[i].ID || replies[1].ID == replies[2].ID {
			t.Errorf("reply %d: id %q (replay %q) — want a derived id, distinct and stable", i, replies[i].ID, again[i].ID)
		}
	}
}

// A rollout that was replaced is read again from the top, and because the ids are Codex's
// own, what was already sent is sent as itself. A batch that fills is a backlog, not a loss.
func TestCodexRepliesSurviveARewrittenRolloutAndABacklog(t *testing.T) {
	var body strings.Builder
	body.WriteString(replyTaskStarted(replyTurnA, replyTraceA))
	for i := range codexReplyBatch + 5 {
		body.WriteString(replyMessage("assistant", fmt.Sprintf("msg_%03d", i), "commentary", "2026-09-19T18:18:41.000Z", fmt.Sprintf("step %d", i)))
	}
	path := sequenceFixture(t, body.String())
	cursor, status, first := readReplies(t, path, CodexReplyCursor{}, 1<<10)
	if status != "backlog" || len(first) != codexReplyBatch {
		t.Fatalf("status %s, %d replies", status, len(first))
	}
	cursor, status, rest := readReplies(t, path, cursor, 1<<10)
	if status != "caught_up" || len(rest) != 5 || rest[0].ID != fmt.Sprintf("msg_%03d", codexReplyBatch) || rest[0].TraceID != replyTraceA {
		t.Fatalf("status %s, rest %+v", status, rest)
	}

	writeEvidenceFile(t, path, fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"rewritten\":true}}\n", testCodexID)+
		replyTaskStarted(replyTurnA, replyTraceA)+replyMessage("assistant", "msg_000", "commentary", "2026-09-19T18:18:41.000Z", "step 0"))
	_, status, replayed := readReplies(t, path, cursor, 1<<10)
	if status != "caught_up" || len(replayed) != 1 || replayed[0].ID != "msg_000" {
		t.Fatalf("after a rewrite: status %s, %+v", status, replayed)
	}
}

// The caller spools before it acknowledges: a failed append leaves the cursor before the
// reply, so the next hook sends it.
func TestCodexRepliesAreNotAcknowledgedUntilSpooled(t *testing.T) {
	path := sequenceFixture(t, replyTaskStarted(replyTurnA, replyTraceA)+
		replyMessage("assistant", "msg_a1", "commentary", "2026-09-19T18:18:41.000Z", "one")+
		replyMessage("assistant", "msg_a2", "final_answer", "2026-09-19T18:18:42.000Z", "two"))
	calls := 0
	cursor, status, err := ReadCodexReplies(context.Background(), testCodexID, path, CodexReplyCursor{}, 1<<10, func(CodexReply) error {
		if calls++; calls == 2 {
			return errors.New("spool is full")
		}
		return nil
	})
	if status != "append_failed" || err == nil {
		t.Fatalf("status %s err %v", status, err)
	}
	_, _, replies := readReplies(t, path, cursor, 1<<10)
	if len(replies) != 1 || replies[0].ID != "msg_a2" {
		t.Fatalf("the reply that was not spooled should be read again, alone: %+v", replies)
	}
}

func TestCodexTraceID(t *testing.T) {
	for id, want := range map[string]bool{
		replyTraceA: true, strings.Repeat("0", 32): false, "AE2A5E6F8E0168AE5EC2EFA2C8772B02": false,
		replyTraceA[:31]: false, replyTurnA: false, "": false,
	} {
		if codexTraceID(id) != want {
			t.Errorf("codexTraceID(%q) = %v", id, !want)
		}
	}
}
