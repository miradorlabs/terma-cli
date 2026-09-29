package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A session start tells the relay which repository the session runs in, for Claude Code
// and Codex alike — the one fact the relay cannot read from an agent's export.
func TestSessionStartRecordsTheSessionForTheRelay(t *testing.T) {
	root := initRepo(t) // gives the test its own config directory
	cfg := os.Getenv("TERMA_CONFIG_DIR")
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := relay.Ensure(); err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	ctx := context.Background()
	const claude, codex = "11111111-1111-4111-8111-111111111111", "01a0edc9-7663-75d0-8a00-5ee4a6d0a5ef"
	if err := SessionStart(ctx, env(`{"session_id":"`+claude+`","cwd":"`+root+`","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	if err := CodexSessionStart(ctx, env(`{"session_id":"`+codex+`","cwd":"`+root+`","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{claude, codex} {
		data, err := os.ReadFile(filepath.Join(cfg, "relay", "sessions", id))
		if err != nil {
			t.Fatalf("%s not recorded: %v", id, err)
		}
		if got := strings.TrimSpace(string(data)); got != root {
			t.Fatalf("%s recorded as %q, want %q", id, got, root)
		}
	}
}
