package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/api"
)

// runBlameCmd drives `terma blame` from inside a real git repository against a fake
// gateway, authenticated with a server key so no credential file is involved.
func runBlameCmd(t *testing.T, repo string, handler http.HandlerFunc, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Chdir(repo)
	run := termaRun{env: fakeGateway(t, handler)}
	return run.exec(t, append([]string{"-o", "json", "blame"}, args...)...)
}

// blameRepo makes a repo with one commit whose message carries the given trailer
// block, and returns the repo path and the commit's full sha.
func blameRepo(t *testing.T, message string) (repo, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo = t.TempDir()
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		repo = r
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(c.Environ(),
			"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
			"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", message)
	return repo, git("rev-parse", "HEAD")
}

// A blamed commit joins its trailer to the backend record and renders every field.
func TestBlame_JoinsCommitToSession(t *testing.T) {
	repo, sha := blameRepo(t, "add a\n\nAgent-Session-Id: 583683ec-8e0c-4fd5-b1a6-c97b74be7f5e\nAgent-Tool: claude-code")

	var got url.Values
	stdout, _, err := runBlameCmd(t, repo, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		got = r.URL.Query()
		fmt.Fprintf(w, `{"logs":[{"event_name":"terma.commit","time":"2026-09-14T18:44:24.98Z",
			"attributes":{"sha":%q,"session.id":"583683ec-8e0c-4fd5-b1a6-c97b74be7f5e",
			"sessions":"583683ec-8e0c-4fd5-b1a6-c97b74be7f5e","tool":"claude-code",
			"lines_added":"2609","lines_deleted":"246","file_count":"32",
			"branch":"feat/x","repo_url":"https://github.com/miradorlabs/terma-cli",
			"terma.repo":"terma-cli","author_email":"dev@example.com"},
			"resource_attributes":{"mirador.project.id":"p"}}]}`, sha)
	}, sha)
	if err != nil {
		t.Fatal(err)
	}

	// The filter names the sha, and the window is centred on the commit (not on now).
	if f := got.Get("filter"); !strings.Contains(f, `attribute.sha="`+sha+`"`) || !strings.Contains(f, `attribute.event.name="terma.commit"`) {
		t.Errorf("filter = %q", f)
	}
	since, errS := time.Parse(time.RFC3339, got.Get("since"))
	until, errU := time.Parse(time.RFC3339, got.Get("until"))
	if errS != nil || errU != nil {
		t.Fatalf("window not RFC3339: %q..%q", got.Get("since"), got.Get("until"))
	}
	// The window is a tight 2h span bracketing the commit's own time — the commit was
	// just made, so it brackets now — rather than scanning back from now on one side.
	now := time.Now()
	if since.After(now) || until.Before(now) || until.Sub(since) != 2*blameWindow {
		t.Errorf("window %v..%v is not a %v-wide window around the commit", since, until, 2*blameWindow)
	}

	var v blameView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if v.SHA != sha || v.Tool != "claude-code" || v.Source != "claude-code" {
		t.Errorf("view head = %+v", v)
	}
	if v.SessionID != "583683ec-8e0c-4fd5-b1a6-c97b74be7f5e" || len(v.Sessions) != 1 {
		t.Errorf("sessions = %+v", v)
	}
	if v.LinesAdded != 2609 || v.LinesDeleted != 246 || v.FileCount != 32 {
		t.Errorf("lines = +%d/-%d over %d", v.LinesAdded, v.LinesDeleted, v.FileCount)
	}
}

// blame defaults to HEAD, and an Agent-Tool with a version yields the bare source.
func TestBlame_DefaultsToHeadAndParsesToolVersion(t *testing.T) {
	repo, sha := blameRepo(t, "work\n\nAgent-Session-Id: s1\nAgent-Tool: codex/0.5.1")
	stdout, _, err := runBlameCmd(t, repo, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("filter"); !strings.Contains(q, sha) {
			t.Errorf("HEAD did not resolve to the commit sha: %q", q)
		}
		fmt.Fprintf(w, `{"logs":[{"event_name":"terma.commit","attributes":{"sha":%q,"session.id":"s1","sessions":"s1","tool":"codex/0.5.1"}}]}`, sha)
	}) // no arg => HEAD
	if err != nil {
		t.Fatal(err)
	}
	var v blameView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatal(err)
	}
	if v.Tool != "codex/0.5.1" || v.Source != "codex" {
		t.Errorf("tool/source = %q / %q", v.Tool, v.Source)
	}
}

// A commit with no trailer is not terma's to attribute, and the miss says so without
// blaming the backend.
func TestBlame_NoTrailerIsClearMiss(t *testing.T) {
	repo, sha := blameRepo(t, "a hand-written commit")
	_, _, err := runBlameCmd(t, repo, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"logs":[]}`)
	}, sha)
	if err == nil || !strings.Contains(err.Error(), "no Agent-Session-Id trailer") {
		t.Fatalf("err = %v", err)
	}
}

// A stamped commit the backend has not seen is a delivery gap, and the miss points at
// the spool rather than at attribution.
func TestBlame_StampedButUnreportedPointsAtSpool(t *testing.T) {
	repo, sha := blameRepo(t, "work\n\nAgent-Session-Id: s1\nAgent-Tool: claude-code")
	_, _, err := runBlameCmd(t, repo, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"logs":[]}`)
	}, sha)
	if err == nil || !strings.Contains(err.Error(), "spool flush") {
		t.Fatalf("err = %v", err)
	}
}

func TestBlame_UnknownRevisionIsAnError(t *testing.T) {
	repo, _ := blameRepo(t, "work\n\nAgent-Session-Id: s1")
	_, _, err := runBlameCmd(t, repo, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("no backend call should happen for an unknown revision")
	}, "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "no such commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestBlameInCommandTree(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"blame"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "blame" || cmd.RunE == nil {
		t.Fatalf("blame resolves to %q with RunE=%v", cmd.CommandPath(), cmd.RunE != nil)
	}
}

// A commit made in a linked worktree says so: the record's worktree travels to the view
// and the repository line, next to the repository it belongs to.
func TestBlameNamesTheWorktree(t *testing.T) {
	rec := &api.LogRecord{Attributes: map[string]any{
		"sha": "abc1234def", "terma.repo": "terma-cli", "branch": "feature", "worktree": "terma-cli-wt-check",
	}}
	v := blameViewOf(rec, "abc1234def")
	if v.Repository != "terma-cli" || v.Worktree != "terma-cli-wt-check" {
		t.Fatalf("view %+v", v)
	}
	var repoRow string
	for _, row := range blameTable(v).Rows {
		if row[0] == "repo" {
			repoRow = row[1]
		}
	}
	if repoRow != "terma-cli @ feature (worktree terma-cli-wt-check)" {
		t.Fatalf("repo row %q", repoRow)
	}
	if v := blameViewOf(&api.LogRecord{Attributes: map[string]any{"terma.repo": "terma-cli"}}, "abc"); v.Worktree != "" {
		t.Fatalf("a main checkout's commit names a worktree: %+v", v)
	}
}
