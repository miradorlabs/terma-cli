package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// testdata/codex_rollout_resumed.jsonl is a real codex-cli 0.159.3 rollout, trimmed: turn 1
// ran in one working copy, `codex exec resume` ran turn 2 in another.
const (
	resumedTurn1 = "01a103c9-6a50-7041-81d0-2f7d4ad05581"
	resumedTurn2 = "01a103c9-f2e2-7233-a90b-7fdc1beaabc2"
)

// stopResumed writes the resumed rollout with turn 1 in first and turn 2 in second, names
// the thread during turn 1, runs Stop in the listed working copy and returns the spool.
func stopResumed(t *testing.T, env hookrun.Env, first, second string) []spool.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_resumed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The working copies go into JSON strings, so a Windows path's backslashes are escaped.
	inJSON := func(s string) string { q, _ := json.Marshal(s); return string(q[1 : len(q)-1]) }
	rollout := strings.NewReplacer("{{UNLISTED}}", inJSON(first), "{{LISTED}}", inJSON(second), "{{ID}}", replySession).Replace(string(b))
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "10", "03", "rollout-2026-10-03T23-01-33-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), rollout)
	titleIndex(t, titleLine(replySession, "P8 UNLISTEDCODEX thread", "2026-10-03T22:01:36Z"))
	in, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6-astra"})
	env.Stdin = strings.NewReader(string(in))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	return hookruntest.Spooled(t, env.Spool)
}

// A thread resumed in a listed repository sends nothing its earlier, unlisted turn
// produced: no reply, quota snapshot, or the name it was given.
func TestCodexResumedThreadSendsNothingFromItsUnlistedTurn(t *testing.T) {
	other := hookruntest.InitRepo(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	evs := stopResumed(t, env, other, env.Cwd)

	raw, _ := json.Marshal(evs)
	for _, leak := range []string{"UNLISTEDCODEX", resumedTurn1, "22:01:"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the unlisted turn left: %q in %s", leak, raw)
		}
	}
	for name, want := range map[string]int{
		semconv.TermaAssistantMessageEvent: 2, semconv.TermaSessionTitleEvent: 0,
	} {
		if got := len(hookruntest.Named(evs, name)); got != want {
			t.Errorf("%s: %d, want %d from the listed turn", name, got, want)
		}
	}
	if len(hookruntest.Named(evs, semconv.TermaSessionQuotaEvent)) == 0 {
		t.Error("the listed turn's quota was not sent")
	}
	for _, e := range hookruntest.Named(evs, semconv.TermaAssistantMessageEvent) {
		if e.Attrs[semconv.TermaTurnIDKey] != resumedTurn2 {
			t.Errorf("%s from turn %v, want the listed turn", e.Name, e.Attrs[semconv.TermaTurnIDKey])
		}
	}
}

// A thread listed from its first turn loses nothing.
func TestCodexListedThreadSendsEveryTurn(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	evs := stopResumed(t, env, env.Cwd, env.Cwd)
	for name, want := range map[string]int{
		semconv.TermaAssistantMessageEvent: 4, semconv.TermaSessionTitleEvent: 1, semconv.TermaSessionQuotaEvent: 4,
	} {
		if got := len(hookruntest.Named(evs, name)); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}
}

// A withheld turn stays withheld across reads: a backlog that ends inside it, then a listed
// turn after it. A turn without a turn_context, as review and compaction turns are, is
// judged by where the thread runs: kept in the listed repository, withheld once a resume
// announces another directory.
func TestCodexRolloutReadersWithholdUnlistedTurnsAcrossReads(t *testing.T) {
	admits := func(cwd string) bool { return cwd == "listed" }
	turn := func(id, cwd string, n int, text string) string {
		var b strings.Builder
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`+"\n", id)
		b.WriteString(turnContext(id, cwd))
		for i := range n {
			b.WriteString(replyMessage("assistant", fmt.Sprintf("msg_%s_%d", cwd, i), "commentary", "2026-10-03T22:01:39Z", text))
			fmt.Fprintf(&b, `{"timestamp":"2026-10-03T22:01:40Z","type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"Extension","id":"item_%s_%d","kind":"web.search"}}}`+"\n", id, cwd, i)
			b.WriteString(quotaRecord(testCodexLimits))
		}
		return b.String() + fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":%q}}`+"\n", id)
	}
	review := func(id, text string) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`+"\n", id) +
			replyMessage("assistant", "msg_"+id, "final_answer", "2026-10-03T22:01:39Z", text) +
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"Extension","id":"item_%s","kind":"web.search"}}}`+"\n", id, id) +
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":%q}}`+"\n", id)
	}
	bare := func(id, text string) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`+"\n"+`{"type":"turn_context","payload":{"turn_id":%q}}`+"\n", id, id) +
			replyMessage("assistant", "msg_"+id, "final_answer", "2026-10-03T22:01:39Z", text)
	}
	resumed := func(cwd string) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"cwd":%q}}}`+"\n", cwd)
	}
	path := sequenceFixture(t, turn("turn-a", "listed", 1, "kept")+turn("turn-b", "elsewhere", codexReplyBatch+5, "UNLISTED")+turn("turn-c", "listed", 1, "kept")+
		review("turn-d", "kept review")+bare("turn-d2", "kept without a cwd")+resumed("elsewhere")+
		`{"timestamp":"2026-10-03T22:01:40Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","id":"item_between","kind":"web.search"}}}`+"\n"+review("turn-e", "UNLISTED review"))

	var replies []reply
	rc, status := replyCursor{}, ""
	for reads := 0; status != "caught_up"; reads++ {
		if reads > 3 {
			t.Fatalf("replies never caught up: %s", status)
		}
		var err error
		rc, status, err = readRolloutReplies(context.Background(), testCodexID, path, rc, 1<<10, admits, func(r reply) error { replies = append(replies, r); return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(replies) != 4 || replies[0].TurnID != "turn-a" || replies[1].TurnID != "turn-c" || replies[2].Text != "kept review" || replies[3].Text != "kept without a cwd" || !rc.Withheld {
		t.Fatalf("replies %+v, withheld %v", replies, rc.Withheld)
	}

	var activity []desktopActivity
	dc, status, err := readDesktopActivity(context.Background(), testCodexID, path, desktopCursor{}, admits, func(a desktopActivity) error { activity = append(activity, a); return nil })
	if err != nil || status != "caught_up" {
		t.Fatalf("desktop activity: %s %v", status, err)
	}
	for _, a := range activity {
		if a.TurnID != "turn-a" && a.TurnID != "turn-c" && a.TurnID != "turn-d" {
			t.Fatalf("activity from a withheld turn: %+v", a)
		}
	}
	if len(activity) != 3 || dc.Admitted {
		t.Fatalf("activity %+v, admitted %v", activity, dc.Admitted)
	}

	var quotas []hookrun.FundingEvidence
	_, _, err = readFunding(context.Background(), testCodexID, path, quotaCursor{}, admits, func(e hookrun.FundingEvidence) error { quotas = append(quotas, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range quotas {
		if q.Attrs[semconv.TermaTurnIDKey] != "turn-a" && q.Attrs[semconv.TermaTurnIDKey] != "turn-c" {
			t.Fatalf("quota from a withheld turn: %+v", q.Attrs)
		}
	}
	if len(quotas) != 2 {
		t.Fatalf("quotas %d, want one per listed turn", len(quotas))
	}
}

// The name waits while the rollout ends mid-record, which may be the turn_context that
// withholds the newest turn.
func TestCodexTitleWaitsForAWholeRollout(t *testing.T) {
	other := hookruntest.InitRepo(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := replyRollout(t, env.Cwd)
	titleIndex(t, titleLine(replySession, "UNLISTEDCODEX thread", "2026-09-19T18:20:00Z"))
	tail := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}` + "\n" + turnContext("turn-2", other)
	titles := func() int {
		in, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path})
		env.Stdin = strings.NewReader(string(in))
		if err := stop(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		return len(hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent))
	}
	appendRollout(t, path, tail[:len(tail)-10])
	if n := titles(); n != 0 {
		t.Fatalf("the name left while the newest turn's context was half-written: %d", n)
	}
	appendRollout(t, path, tail[len(tail)-10:])
	if n := titles(); n != 0 {
		t.Fatalf("the name left once a turn ran where the team does not collect: %d", n)
	}
}

// A turn a read admitted is judged again by the next: the policy may have dropped it in
// between. Withheld survives a rewritten rollout, and a gap in a withheld turn does not
// name it.
func TestCodexRolloutAdmissionIsRecheckedAndSticky(t *testing.T) {
	listed := true
	admits := func(cwd string) bool { return cwd == "repo" && listed }
	var body strings.Builder
	body.WriteString(replyTaskStarted(replyTurnA, replyTraceA) + replyTurnContext(replyTurnA))
	for i := range codexReplyBatch + 5 {
		body.WriteString(replyMessage("assistant", fmt.Sprintf("msg_%03d", i), "commentary", "2026-09-19T18:18:41.000Z", "step"))
	}
	body.WriteString("{not json\n")
	path := sequenceFixture(t, body.String())
	read := func(c replyCursor) (replyCursor, int) {
		n := 0
		next, _, err := readRolloutReplies(context.Background(), testCodexID, path, c, 1<<10, admits, func(reply) error { n++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		return next, n
	}
	c, n := read(replyCursor{})
	if n != codexReplyBatch || !c.Admitted || c.Withheld {
		t.Fatalf("first read: %d replies, cursor %+v", n, c)
	}
	listed = false
	if c, n = read(c); n != 0 || !c.Withheld {
		t.Fatalf("after the policy dropped the repository: %d replies, cursor %+v", n, c)
	}
	writeEvidenceFile(t, path, fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"rewritten\":true}}\n", testCodexID))
	if c, _ = read(c); !c.Withheld {
		t.Fatal("a rewritten rollout forgot the withheld turn")
	}

	listed = true
	path = sequenceFixture(t, replyTaskStarted(replyTurnA, replyTraceA)+turnContext(replyTurnA, "elsewhere")+"{not json\n")
	_, _, err := readFunding(context.Background(), testCodexID, path, quotaCursor{}, admits, func(e hookrun.FundingEvidence) error {
		if _, ok := e.Attrs[semconv.TermaTurnIDKey]; ok {
			t.Errorf("a gap named a withheld turn: %v", e.Attrs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
