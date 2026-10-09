package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The remote-tracking branch shows a push only when the push went where the fetch comes
// from: one repository, however each URL spells it.
func TestTrackingRefFSFollowsThePushURL(t *testing.T) {
	for _, tc := range []struct {
		what, pushURL string
		ok            bool
	}{
		{what: "no push URL", ok: true},
		{what: "the same repository over SSH", pushURL: "git@github.com:acme/r.git", ok: true},
		{what: "another repository", pushURL: "git@github.com:acme/fork.git"},
		{what: "a local path", pushURL: "/srv/r.git"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			gitDir := t.TempDir()
			config := "[remote \"origin\"]\n\turl = https://github.com/acme/r.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
			if tc.pushURL != "" {
				config += "\tpushurl = " + tc.pushURL + "\n"
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			ref, ok := TrackingRefFS(gitDir, "origin", "refs/heads/main")
			if ok != tc.ok || (ok && ref != "refs/remotes/origin/main") {
				t.Errorf("got %q, %v; want ok %v", ref, ok, tc.ok)
			}
		})
	}
}

// Each commit's session and tool trailers come back in their order, so a tool stays with
// the session before it, and no other trailer comes with them.
func TestCommitsKeepEachToolWithItsSession(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	msg := "mixed\n\nAgent-Session-Id: 5e757e6f-3040-4c57-b37d-01d44cc43053\nAgent-Tool: claude-code/2.1.3\n" +
		"Signed-off-by: Dev <dev@example.com>\nAgent-Session-Id: 5e757e6f-3040-4c57-b37d-01d44cc43053\nAgent-Tool: codex/0.160.1\n"
	if _, err := Git(ctx, dir, "commit", "-q", "--allow-empty", "-m", "by hand"); err != nil {
		t.Fatal(err)
	}
	base, _ := Git(ctx, dir, "rev-parse", "HEAD")
	if _, err := Git(ctx, dir, "commit", "-q", "--allow-empty", "-m", msg); err != nil {
		t.Fatal(err)
	}
	commits, err := Commits(ctx, dir, "HEAD", []string{strings.TrimSpace(base)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "Agent-Session-Id: 5e757e6f-3040-4c57-b37d-01d44cc43053\nAgent-Tool: claude-code/2.1.3\n" +
		"Agent-Session-Id: 5e757e6f-3040-4c57-b37d-01d44cc43053\nAgent-Tool: codex/0.160.1\n"
	if len(commits) != 1 || !ValidOID(commits[0].SHA) || commits[0].Trailers != want {
		t.Fatalf("commits = %+v", commits)
	}
}
