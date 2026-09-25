package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
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
	writeFile(t, main, ".terma/settings.json", `{"project":{"id":"proj-main"}}`)
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
	writeFile(t, wt, "src/agent.go", "package src\n")
	run(wt, `{"session_id":"sess-wt","cwd":"`+wt+`","hook_event_name":"SessionStart","source":"startup"}`, SessionStart)
	run(wt, `{"session_id":"sess-wt","cwd":"`+wt+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(wt, "src", "agent.go")+`"}}`, PostToolUse)
	run(main, `{"session_id":"sess-main","cwd":"`+main+`","hook_event_name":"SessionStart","source":"startup"}`, SessionStart)

	events := spooled(t, sp)
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

// Codex Desktop's activity (model calls, turn summaries, compactions) is spooled without
// emitFor, and went without a project id — so the flush dropped every one as
// unroutable. It carries the repository's binding like every other event.
func TestCodexDesktopActivityCarriesTheProject(t *testing.T) {
	env := fundingEnv(t)
	connectCodexDesktop(t, false)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	writeFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		`{"type":"turn_context","payload":{"turn_id":"` + replyTurn + `","model":"gpt-6-sol"}}`,
		`{"timestamp":"2026-09-19T18:18:42Z","type":"token_usage_record","payload":{"turn_id":"` + replyTurn + `","response_id":"resp_1","usage":{"input_tokens":100,"output_tokens":40}}}`,
		`{"timestamp":"2026-09-19T18:18:44Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `","duration_ms":4400}}`,
	}, "\n")+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6-sol"})
	env.Stdin = strings.NewReader(string(b))
	if err := CodexStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	activity := 0
	for _, e := range spooled(t, env.Spool) {
		if e.Name != EventModelCall && e.Name != EventTurnSummary {
			continue
		}
		activity++
		if e.Attrs[AttrProjectID] != "project-a" {
			t.Fatalf("%s spooled without its project: %v", e.Name, e.Attrs)
		}
	}
	if activity == 0 {
		t.Fatal("no Desktop activity was captured")
	}
}
