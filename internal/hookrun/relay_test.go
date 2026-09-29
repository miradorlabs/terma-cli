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
		// One placement: "<unix nanos>\t<directory>".
		if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, "\t"+root) || strings.Count(got, "\n") != 0 {
			t.Fatalf("%s recorded as %q, want one placement in %q", id, got, root)
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

// The user-level placement hook records where a session runs in any directory: a
// repository's root from inside it, and a personal directory as itself.
func TestPlaceRecordsWhereASessionRuns(t *testing.T) {
	root := initRepo(t)
	cfg := os.Getenv("TERMA_CONFIG_DIR")
	if _, err := relay.Ensure(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	personal := t.TempDir()
	const id = "11111111-1111-4111-8111-111111111111"
	for _, cwd := range []string{sub, personal} {
		env := Env{Now: time.Now(), Cwd: cwd, Stdin: strings.NewReader(`{"session_id":"` + id + `","cwd":"` + cwd + `","hook_event_name":"SessionStart","source":"resume"}`)}
		if err := Place(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(cfg, "relay", "sessions", id))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "\t"+root) || !strings.HasSuffix(lines[1], "\t"+personal) {
		t.Fatalf("placements:\n%s\nwant the repository root, then the personal directory", data)
	}
}
