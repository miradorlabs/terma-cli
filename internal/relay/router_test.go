package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestRouter is a router over dir with a clock the test moves.
func newTestRouter(t *testing.T, dir string, hold time.Duration, clock *time.Time, bindings map[string]string) (*router, map[string]int) {
	t.Helper()
	routed := map[string]int{}
	binding := func(d string) (string, error) { return bindings[d], nil }
	return &router{
		dir: dir, res: newResolver(dir, binding, nil, t.Logf), traces: newTraceMap(64),
		hold: hold, traceHold: hold, now: func() time.Time { return *clock }, logf: t.Logf, stats: newStats(),
		routed: func(r string) { routed[r]++ },
	}, routed
}

func spansBody(t *testing.T, spans ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"scopeSpans": []any{map[string]any{"spans": spans}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A Codex Desktop turn's spans wait on a trace no record has named yet. While nothing
// changes, the router neither re-reads nor rewrites them; the log that names the trace's
// session releases them on the next pass; and the hold closing sends what is left to
// the machine project.
func TestWaitingEntriesAreLeftAloneUntilSomethingChanges(t *testing.T) {
	dir := t.TempDir()
	start := time.Unix(1790700000, 0)
	clock := start
	rt, routed := newTestRouter(t, dir, 30*time.Second, &clock, map[string]string{repoPath("codex"): "proj-codex"})
	ctx := context.Background()
	inbox := filepath.Join(dir, inboxDir)

	waiting := newEntry(start, sigTraces, formatJSON)
	if err := writeEntry(inbox, waiting, spansBody(t,
		map[string]any{"name": "run_turn", "traceId": "trace-1"},
		map[string]any{"name": "orphan", "traceId": "trace-never"},
	)); err != nil {
		t.Fatal(err)
	}
	rt.pass(ctx)
	if rt.looked != 1 || len(routed) != 0 {
		t.Fatalf("first pass: looked %d, routed %v", rt.looked, routed)
	}
	info, _ := os.Stat(filepath.Join(inbox, waiting.name))

	for range 3 {
		clock = clock.Add(time.Second)
		rt.pass(ctx)
	}
	if rt.looked != 1 {
		t.Fatalf("an entry nothing changed about was read %d more times", rt.looked-1)
	}
	if now, _ := os.Stat(filepath.Join(inbox, waiting.name)); !now.ModTime().Equal(info.ModTime()) {
		t.Fatal("an entry nothing changed about was rewritten")
	}

	// The session's log arrives, naming trace-1; the hook recorded its directory.
	if err := os.MkdirAll(filepath.Join(dir, sessionsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionsDir, codexID), []byte(repoPath("codex")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeEntry(inbox, newEntry(clock, sigLogs, formatJSON),
		logsBody(t, logRecord("codex.user_prompt", "trace-1", strAttr("conversation.id", codexID)))); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	rt.pass(ctx) // routes the log and learns trace-1
	rt.pass(ctx) // the waiting spans are due: their trace is known now
	if routed["proj-codex"] < 2 {
		t.Fatalf("run_turn not released to its session's project: %v", routed)
	}
	left, _ := listEntries(inbox)
	if len(left) != 1 || left[0].name != waiting.name {
		t.Fatalf("inbox %v, want only the orphan's entry", left)
	}

	clock = start.Add(31 * time.Second)
	rt.pass(ctx)
	if left, _ := listEntries(inbox); len(left) != 0 || routed[machineRoute] != 1 {
		t.Fatalf("after the hold: inbox %v, routed %v", left, routed)
	}
}

// A session a hook records while its records wait is picked up before the recheck.
func TestAHookRecordReleasesAWaitingSessionEarly(t *testing.T) {
	dir := t.TempDir()
	clock := time.Unix(1790700000, 0)
	rt, routed := newTestRouter(t, dir, 30*time.Second, &clock, map[string]string{repoPath("a"): "proj-a"})
	ctx := context.Background()
	if err := writeEntry(filepath.Join(dir, inboxDir), newEntry(clock, sigLogs, formatJSON),
		logsBody(t, logRecord("early", "", strAttr("session.id", sessionA)))); err != nil {
		t.Fatal(err)
	}
	rt.pass(ctx)
	clock = clock.Add(time.Second)
	rt.pass(ctx)
	if rt.looked != 1 {
		t.Fatalf("looked %d times before anything changed", rt.looked)
	}
	if err := os.MkdirAll(filepath.Join(dir, sessionsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionsDir, sessionA), []byte(repoPath("a")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	rt.pass(ctx)
	if routed["proj-a"] != 1 {
		t.Fatalf("routed %v, want the session's record in proj-a before the recheck", routed)
	}
}
