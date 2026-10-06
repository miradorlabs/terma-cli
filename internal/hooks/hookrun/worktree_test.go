package hookrun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A linked worktree's events go to the developer's team, as the main checkout's do.
func TestWorktreeEventsReportTheMainRepositoryAndProject(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	main := hookruntest.InitRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "feature-x")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	wt, _ = filepath.EvalSymlinks(wt)

	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pol := hookruntest.Admitting(main)
	run := func(cwd, payload string, hook func(context.Context, Env) error) {
		t.Helper()
		if err := hook(ctx, Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Policy: pol, Stdin: strings.NewReader(payload), Spool: sp, Version: "test", Team: "proj-main"}); err != nil {
			t.Fatal(err)
		}
	}
	hookruntest.WriteFile(t, wt, "src/agent.go", "package src\n")
	run(wt, `{"session_id":"sess-wt","cwd":"`+hookruntest.InJSON(wt)+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)
	run(wt, `{"session_id":"sess-wt","cwd":"`+hookruntest.InJSON(wt)+`","tool_name":"Write","tool_input":{"file_path":"`+hookruntest.InJSON(filepath.Join(wt, "src", "agent.go"))+`"}}`, editFile)
	run(main, `{"session_id":"sess-main","cwd":"`+hookruntest.InJSON(main)+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)

	events := hookruntest.Spooled(t, sp)
	if len(events) < 3 {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events {
		if e.Attrs[AttrProjectID] != "proj-main" {
			t.Fatalf("%s from %s: attrs %v", e.Name, e.SessionID, e.Attrs)
		}
	}
}
