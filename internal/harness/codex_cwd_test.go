package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexRolloutCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	const id = "01a0edc9-7663-75d0-8a00-5ee4a6d0a5ef"
	dir := filepath.Join(home, "sessions", "2026", "09", "29")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, head string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(head+"\n{\"type\":\"response_item\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rollout-2026-09-29T09-30-03-"+id+".jsonl",
		`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"/Users/dev/repo/sub","originator":"Codex Desktop"}}`)
	if cwd, status := CodexRolloutCwd(context.Background(), id, ""); cwd != "/Users/dev/repo/sub" || status != "present" {
		t.Fatalf("got (%q, %q)", cwd, status)
	}

	// A rollout whose header names another thread is not this session's.
	const other = "01a0edc9-0000-7000-8000-000000000000"
	write("rollout-2026-09-29T09-31-00-"+other+".jsonl",
		`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"/elsewhere"}}`)
	if cwd, status := CodexRolloutCwd(context.Background(), other, ""); cwd != "" || status != "session_mismatch" {
		t.Fatalf("mismatched header: (%q, %q)", cwd, status)
	}
	if cwd, status := CodexRolloutCwd(context.Background(), "01a0edc9-1111-7111-8111-111111111111", ""); cwd != "" || status != "missing" {
		t.Fatalf("unknown thread: (%q, %q)", cwd, status)
	}
}

// A thread resumed elsewhere writes that turn's directory in a turn_context; the
// reader finds it from where it left off, decodes nothing else, and never consumes a
// line still being written.
func TestCodexRolloutDirsReadsTurnsIncrementally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	const id = "01a0eebf-4fee-7fe2-9f3f-d81e9276dfe3"
	dir := filepath.Join(home, "sessions", "2026", "09", "29")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-09-29T19-58-31-"+id+".jsonl")
	write := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"timestamp":"2026-09-29T19:58:31.115Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/repos/a"}}` + "\n")
	write(`{"timestamp":"2026-09-29T19:58:32.762Z","type":"turn_context","payload":{"cwd":"/repos/a","turn_id":"t1"}}` + "\n")
	write(`{"timestamp":"2026-09-29T19:58:33.000Z","type":"response_item","payload":{"type":"message","content":"a turn_context-looking \"cwd\":\"/secret\" inside what was said"}}` + "\n")
	ctx := context.Background()
	dirs, next := CodexRolloutDirs(ctx, id, 0)
	if len(dirs) != 2 || dirs[0].Cwd != "/repos/a" || !dirs[0].At.IsZero() || dirs[1].At.IsZero() {
		t.Fatalf("first read: %+v", dirs)
	}
	write(`{"timestamp":"2026-09-29T19:58:34.562Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"` + id + `","thread_settings":{"model":"m","cwd":"/home/dev/personal"}}}` + "\n")
	write(`{"timestamp":"2026-09-29T19:58:36.687Z","type":"turn_context","payload":{"cwd":"/home/dev/personal","turn_id":"t2"}}` + "\n")
	write(`{"timestamp":"2026-09-29T19:58:37.000Z","type":"turn_context","payload":{"cwd":"/half/writ`) // no newline yet
	dirs, next2 := CodexRolloutDirs(ctx, id, next)
	if len(dirs) != 2 || dirs[0].Cwd != "/home/dev/personal" || dirs[0].At.Format(time.RFC3339Nano) != "2026-09-29T19:58:34.562Z" || next2 <= next {
		t.Fatalf("second read from %d: %+v (next %d)", next, dirs, next2)
	}
	if dirs, _ := CodexRolloutDirs(ctx, id, next2); len(dirs) != 0 {
		t.Fatalf("a line still being written was read: %+v", dirs)
	}
}
