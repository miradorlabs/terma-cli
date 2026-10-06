package e2e

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestClaudeEditInLinkedWorktree is a Claude Code session in a linked git worktree — the
// shape `git worktree add` and Claude Code's own isolated worktrees leave. The worktree
// reads origin from its main repository, which the team lists, so it is admitted. Its
// events must reach the team's project, naming neither checkout's directory; a commit made
// there runs terma's global git hooks and must be stamped and delivered on its branch.
func TestClaudeEditInLinkedWorktree(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		mode, route := claudeMode(t)
		sb := New(t, mode, WithClaude(b))
		wt := filepath.Join(sb.Dir, "feature-wt")
		sb.git("worktree", "add", "-q", "-b", "feature", wt)

		// The session and the commit happen in the worktree.
		sb.Repo = wt
		run := sb.ClaudeInteractive(route,
			"Use the Write tool to create a file named hello.txt whose entire content is the word hello. Then reply with exactly TERMA_OK.",
			termaOK, "--allowedTools", "Write", "--tools", "Write", "--permission-mode", "acceptEdits")
		sid := run.SessionID

		// Delivered, not merely spooled: the flush drops an event with no project, so
		// arriving at the receiver is the evidence the worktree was admitted.
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
			for _, k := range []string{"terma.repo", "worktree", "project_id"} {
				if _, ok := r.Attrs[k]; ok {
					t.Errorf("%s still carries %s: %v", name, k, r.Attrs)
				}
			}
		}
		check("terma.session.start", sb.Delivered("terma.session.start", sid, 30*time.Second))
		files := sb.Delivered("terma.files.touched", sid, 30*time.Second)
		check("terma.files.touched", files)
		if len(files) > 0 && !slices.Contains(listAttr(files[0].Attrs["terma.files.paths"]), "hello.txt") {
			t.Errorf("files touched = %q", files[0].Attrs["terma.files.paths"])
		}

		msg := sb.Commit("add hello from the worktree")
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) {
			t.Errorf("worktree commit not stamped with the session:\n%s", msg)
		}
		commits := sb.Delivered("terma.commit", sid, 30*time.Second)
		check("terma.commit", commits)
		if len(commits) > 0 && (commits[0].Attrs["vcs.ref.head.name"] != "feature" || commits[0].Attrs["vcs.ref.head.type"] != "branch") {
			t.Errorf("worktree commit's branch: %v", commits[0].Attrs)
		}
		AddSpend(SpendOf(sb.APIRequests(sid, 20*time.Second)))
	})
}
