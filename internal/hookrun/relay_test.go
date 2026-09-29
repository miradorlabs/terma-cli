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

// Under the relay, terma's own read of Codex's replies follows the project's content
// policy — its own, else the machine default — and a machine with neither reads nothing.
func TestCodexReplyConsentFollowsTheRelaysContentPolicy(t *testing.T) {
	root := initRepo(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := relay.Ensure(); err != nil {
		t.Fatal(err)
	}
	r := &repo{root: root, projectID: "proj-a"}
	if codexRepliesConsented(r) {
		t.Fatal("no content policy recorded, yet replies were read")
	}
	if err := relay.SaveContentPolicy(relay.MachineRoute, relay.ContentPolicy{Prompts: true}); err != nil {
		t.Fatal(err)
	}
	if !codexRepliesConsented(r) {
		t.Fatal("the machine default allows prompts; a project with no policy of its own takes it")
	}
	if err := relay.SaveContentPolicy("proj-a", relay.ContentPolicy{Prompts: false}); err != nil {
		t.Fatal(err)
	}
	if codexRepliesConsented(r) {
		t.Fatal("the project withholds prompts; its replies were read anyway")
	}
}
