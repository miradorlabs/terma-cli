package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
