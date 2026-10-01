package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClaudeEditInLinkedWorktree is a Claude Code session in a linked git worktree of a
// repository that keeps its binding out of git — the shape `git worktree add` and Claude
// Code's own isolated worktrees leave, since neither copies an ignored file. The
// worktree has no .terma/settings.json. Its events must still reach the repository's
// project (an event without one is dropped at the flush), report the main repository as
// terma.repo, and name the worktree; a commit made there must be stamped and delivered.
func TestClaudeEditInLinkedWorktree(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		mode, route := claudeMode(t)
		sb := New(t, mode, WithClaude(b))
		repoName := filepath.Base(sb.Repo)

		// Everything install wrote is committed except the binding, which is ignored.
		sb.write(".gitignore", ".terma/settings.json\n")
		sb.Commit("wire terma hooks; keep the binding local")
		wt := filepath.Join(sb.Dir, "feature-wt")
		sb.git("worktree", "add", "-q", "-b", "feature", wt)
		if _, err := os.Stat(filepath.Join(wt, ".terma", "settings.json")); !os.IsNotExist(err) {
			t.Fatalf("precondition: the worktree must have no binding of its own (stat: %v)", err)
		}
		for _, hook := range []string{".claude/settings.json", ".terma/hooks/post-commit"} {
			if _, err := os.Stat(filepath.Join(wt, hook)); err != nil {
				t.Fatalf("precondition: the worktree must have the committed %s: %v", hook, err)
			}
		}

		// The session and the commit happen in the worktree.
		sb.Repo = wt
		run := sb.ClaudeInteractive(route,
			"Use the Write tool to create a file named hello.txt whose entire content is the word hello. Then reply with exactly TERMA_OK.",
			termaOK, "--allowedTools", "Write", "--tools", "Write", "--permission-mode", "acceptEdits")
		sid := run.SessionID

		// Delivered, not merely spooled: the flush drops an event with no project, so
		// arriving at the receiver is the evidence the binding was found.
		check := func(name string, recs []LogRecord) {
			t.Helper()
			if len(recs) == 0 {
				t.Errorf("%s from the worktree was never delivered; spool: %+v", name, sb.Spool())
				return
			}
			r := recs[0]
			if got := r.Resource["mirador.project.id"]; got != sb.ProjectID {
				t.Errorf("%s: project %q, want %q", name, got, sb.ProjectID)
			}
			if got := r.Attrs["terma.repo"]; got != repoName {
				t.Errorf("%s: terma.repo %q, want the main repository %q", name, got, repoName)
			}
			if got := r.Attrs["worktree"]; got != "feature-wt" {
				t.Errorf("%s: worktree %q, want %q", name, got, "feature-wt")
			}
		}
		check("terma.session.start", sb.Delivered("terma.session.start", sid, 30*time.Second))
		files := sb.Delivered("terma.files.touched", sid, 30*time.Second)
		check("terma.files.touched", files)
		if len(files) > 0 && !strings.Contains(files[0].Attrs["files"], "hello.txt") {
			t.Errorf("files touched = %q", files[0].Attrs["files"])
		}

		msg := sb.Commit("add hello from the worktree")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) {
			t.Errorf("worktree commit not stamped with the session:\n%s", msg)
		}
		check("terma.commit", sb.Delivered("terma.commit", sid, 30*time.Second))
		AddSpend(SpendOf(sb.APIRequests(sid, 20*time.Second)))
	})
}
