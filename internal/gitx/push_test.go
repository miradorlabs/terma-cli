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
// the session before it. The trailers are those of a real mixed commit, 3e6c16f in
// terma-sim-sandbox, whose terma.commit and terma.push reached prd.
func TestCommitsKeepEachToolWithItsSession(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	const trailers = "Agent-Session-Id: 01a11feb-88f3-7f92-9137-144b0ac92b7e\nAgent-Tool: codex\n" +
		"Agent-Session-Id: 0102cabd-6dca-42ce-9888-c0e010ece99f\nAgent-Tool: claude-code\n" +
		"Agent-Session-Id: 01a11fee-3f5c-75a1-80fd-0d26987b1720\nAgent-Tool: codex\n"
	if _, err := Git(ctx, dir, "commit", "-q", "--allow-empty", "-m", "by hand"); err != nil {
		t.Fatal(err)
	}
	base, _ := Git(ctx, dir, "rev-parse", "HEAD")
	if _, err := Git(ctx, dir, "commit", "-q", "--allow-empty", "-m", "pairs: mixed Claude Code and Codex commit\n\n"+trailers); err != nil {
		t.Fatal(err)
	}
	commits, err := Commits(ctx, dir, "HEAD", []string{strings.TrimSpace(base)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !ValidOID(commits[0].SHA) || commits[0].Trailers != trailers {
		t.Fatalf("commits = %+v", commits)
	}
}
