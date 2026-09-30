package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A linked worktree of a repository whose binding is gitignored has none of its own. Its
// events used to carry no project and be dropped at the next flush, and to report the
// worktree's directory as the repository. They carry the main checkout's project, its
// repository name, and which worktree they came from.
func TestWorktreeEventsReportTheMainRepositoryAndProject(t *testing.T) {
	main := initRepo(t)
	ctx := context.Background()
	if _, err := gitx.Git(ctx, main, "commit", "-q", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, main, ".terma/settings.json", `{"project":{"id":"proj-main"}}`)
	wt := filepath.Join(t.TempDir(), "feature-x")
	if _, err := gitx.Git(ctx, main, "worktree", "add", "-q", wt); err != nil {
		t.Fatal(err)
	}
	wt, _ = filepath.EvalSymlinks(wt)
	if _, err := os.Stat(filepath.Join(wt, ".terma", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("precondition: the worktree has no binding (%v)", err)
	}

	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(cwd, payload string, hook func(context.Context, Env) error) {
		t.Helper()
		if err := hook(ctx, Env{Now: time.Now(), Cwd: cwd, Stdin: strings.NewReader(payload), Spool: sp, Version: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	hookruntest.WriteFile(t, wt, "src/agent.go", "package src\n")
	run(wt, `{"session_id":"sess-wt","cwd":"`+wt+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)
	run(wt, `{"session_id":"sess-wt","cwd":"`+wt+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(wt, "src", "agent.go")+`"}}`, editFile)
	run(main, `{"session_id":"sess-main","cwd":"`+main+`","hook_event_name":"SessionStart","source":"startup"}`, startSession)

	events := hookruntest.Spooled(t, sp)
	if len(events) < 3 {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events {
		if e.Attrs[AttrProjectID] != "proj-main" || e.Repo != filepath.Base(main) {
			t.Fatalf("%s from %s: repo %q, attrs %v", e.Name, e.SessionID, e.Repo, e.Attrs)
		}
		wantWorktree := map[string]any{"sess-wt": "feature-x", "sess-main": nil}[e.SessionID]
		if e.Attrs[AttrWorktree] != wantWorktree {
			t.Fatalf("%s from %s: worktree %v, want %v", e.Name, e.SessionID, e.Attrs[AttrWorktree], wantWorktree)
		}
	}
}
