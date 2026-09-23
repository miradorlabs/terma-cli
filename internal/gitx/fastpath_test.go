package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLocateFSMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "") // registers the restore
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mustGit(t, root, "init", "-q")
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gotRoot, gotDir, ok := LocateFS(nested)
	if !ok || gotRoot != root || gotDir != filepath.Join(root, ".git") {
		t.Fatalf("LocateFS(%s) = %q %q %v", nested, gotRoot, gotDir, ok)
	}
	if _, _, ok := LocateFS(t.TempDir()); ok {
		t.Fatal("LocateFS found a repo outside one")
	}

	// A linked worktree has a `.git` file pointing into the main repo.
	mustGit(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, root, "worktree", "add", "-q", "--detach", wt)
	wt, _ = filepath.EvalSymlinks(wt)
	wantRoot, wantDir, err := Locate(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	gotRoot, gotDir, ok = LocateFS(wt)
	if !ok || gotRoot != wantRoot || gotDir != wantDir {
		t.Fatalf("worktree LocateFS = %q %q %v, git says %q %q", gotRoot, gotDir, ok, wantRoot, wantDir)
	}
	if got := CommonDirFS(gotDir); got != filepath.Join(root, ".git") {
		t.Fatalf("CommonDirFS = %q", got)
	}
}

func TestCommentCharFS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	root := t.TempDir()
	mustGit(t, root, "init", "-q")
	gitDir := filepath.Join(root, ".git")
	if got := CommentCharFS(gitDir); got != "#" {
		t.Fatalf("default = %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tcommentChar = \";\" # trailing comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CommentCharFS(gitDir); got != ";" {
		t.Fatalf("global = %q", got)
	}
	mustGit(t, root, "config", "core.commentChar", "auto")
	if got := CommentCharFS(gitDir); got != "#" {
		t.Fatalf("auto = %q", got)
	}
	mustGit(t, root, "config", "core.commentChar", "%")
	if got := CommentCharFS(gitDir); got != "%" {
		t.Fatalf("local = %q", got)
	}
}

func TestLastCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	mustGit(t, root, "init", "-q")
	if _, err := LastCommit(context.Background(), root); err == nil {
		t.Fatal("expected an error on an unborn branch")
	}
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "y.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", ".")
	mustGit(t, root, "-c", "user.email=me@example.com", "-c", "user.name=me", "commit", "-q", "-m", "feat: subject\n\nbody line\n\nAgent-Session-Id: abc\nAgent-Tool: claude-code/2.0")
	c, err := LastCommit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SHA) != 40 || c.AuthorEmail != "me@example.com" {
		t.Fatalf("sha/email = %q %q", c.SHA, c.AuthorEmail)
	}
	if !strings.HasPrefix(c.Message, "feat: subject\n\nbody line") || !strings.HasSuffix(c.Message, "Agent-Tool: claude-code/2.0") {
		t.Fatalf("message = %q", c.Message)
	}
	if strings.Join(c.Paths(), ",") != "dir/y.txt,x.txt" {
		t.Fatalf("files = %v", c.Files)
	}
}

// TestParseNumstat covers the record framing directly: the shapes below are what
// `git log -1 -z --format=...%x1e --numstat` writes after the format block.
func TestParseNumstat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  []FileStat
	}{
		{
			name: "empty commit",
			// The format record's own NUL, and nothing after it.
			block: "\x00",
		},
		{
			name:  "text files",
			block: "\x00\n3\t1\tx.txt\x0012\t0\tdir/y.txt\x00",
			want: []FileStat{
				{Path: "x.txt", Added: 3, Deleted: 1},
				{Path: "dir/y.txt", Added: 12},
			},
		},
		{
			name:  "binary file reports no counts",
			block: "\x00\n-\t-\tlogo.png\x001\t0\tREADME.md\x00",
			want: []FileStat{
				{Path: "logo.png", Binary: true},
				{Path: "README.md", Added: 1},
			},
		},
		{
			name: "rename keeps the following records aligned",
			// The rename spends three records; a parser that splits on NUL alone
			// reads "old.txt" as a stat line and every record after it shifts.
			block: "\x004\t2\t\x00old.txt\x00new.txt\x009\t9\tafter.txt\x00",
			want: []FileStat{
				{Path: "new.txt", Added: 4, Deleted: 2},
				{Path: "after.txt", Added: 9, Deleted: 9},
			},
		},
		{
			name:  "binary rename",
			block: "\x00-\t-\t\x00old.bin\x00new.bin\x00",
			want:  []FileStat{{Path: "new.bin", Binary: true}},
		},
		{
			name:  "tabs and spaces in a path stay in the path",
			block: "\x001\t2\ta b\tc.txt\x00",
			want:  []FileStat{{Path: "a b\tc.txt", Added: 1, Deleted: 2}},
		},
		{
			name:  "truncated rename is dropped, not guessed",
			block: "\x001\t1\t\x00old.txt\x00",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNumstat(tc.block)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("file %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLastCommitFileStats runs the same cases through real repositories, so the
// parser is held to what this machine's git actually prints.
func TestLastCommitFileStats(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	commit := func(t *testing.T, root, msg string) {
		t.Helper()
		mustGit(t, root, "add", "-A")
		mustGit(t, root, "-c", "user.email=me@example.com", "-c", "user.name=me", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", msg)
	}
	write := func(t *testing.T, root, rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		want  []FileStat
	}{
		{
			name: "text files",
			setup: func(t *testing.T, root string) {
				write(t, root, "x.txt", "a\nb\nc\n")
				write(t, root, "dir/y.txt", "y\n")
				commit(t, root, "add")
				write(t, root, "x.txt", "a\nB\nc\nd\n")
				commit(t, root, "edit")
			},
			want: []FileStat{{Path: "x.txt", Added: 2, Deleted: 1}},
		},
		{
			name: "binary file",
			setup: func(t *testing.T, root string) {
				write(t, root, "blob.bin", "\x00\x01\x02binary\x00")
				write(t, root, "README.md", "hello\n")
				commit(t, root, "add")
			},
			want: []FileStat{
				{Path: "README.md", Added: 1},
				{Path: "blob.bin", Binary: true},
			},
		},
		{
			name: "rename with an edit, followed by another file",
			setup: func(t *testing.T, root string) {
				write(t, root, "old.txt", "a\nb\nc\n")
				write(t, root, "zz.txt", "z\n")
				commit(t, root, "add")
				mustGit(t, root, "mv", "old.txt", "new.txt")
				write(t, root, "new.txt", "a\nb\nc\nd\n")
				write(t, root, "zz.txt", "z\nz\n")
				commit(t, root, "rename and edit")
			},
			want: []FileStat{
				{Path: "new.txt", Added: 1},
				{Path: "zz.txt", Added: 1},
			},
		},
		{
			name: "empty commit",
			setup: func(t *testing.T, root string) {
				write(t, root, "a.txt", "a\n")
				commit(t, root, "add")
				commit(t, root, "nothing")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustGit(t, root, "init", "-q", "-b", "main")
			tc.setup(t, root)
			c, err := LastCommit(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Files) != len(tc.want) {
				t.Fatalf("files = %+v, want %+v", c.Files, tc.want)
			}
			for i := range c.Files {
				if c.Files[i] != tc.want[i] {
					t.Fatalf("file %d = %+v, want %+v", i, c.Files[i], tc.want[i])
				}
			}
			if len(c.Paths()) != len(tc.want) {
				t.Fatalf("Paths() = %v", c.Paths())
			}
			for i, p := range c.Paths() {
				if p != tc.want[i].Path {
					t.Fatalf("Paths()[%d] = %q, want %q", i, p, tc.want[i].Path)
				}
			}
		})
	}
}

// post-commit is not told a commit's source the way prepare-commit-msg is, so the
// merge and squash it must skip have to be read off the commit itself: the parent
// count that comes with the log call, and git's default squash subject.
func TestLastCommitTellsMergesAndSquashes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	mustGit(t, root, "init", "-q", "-b", "main")
	mustGit(t, root, "config", "user.email", "me@example.com")
	mustGit(t, root, "config", "user.name", "me")
	mustGit(t, root, "config", "commit.gpgsign", "false")
	commit := func(name, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, root, "add", name)
		mustGit(t, root, "commit", "-q", "-m", msg)
	}
	head := func() Commit {
		t.Helper()
		c, err := LastCommit(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	commit("base", "base")
	if c := head(); len(c.Parents) != 0 || c.IsMerge() || c.IsSquash() {
		t.Fatalf("root commit: parents=%v merge=%v squash=%v", c.Parents, c.IsMerge(), c.IsSquash())
	}
	commit("second", "second")
	if c := head(); len(c.Parents) != 1 || c.IsMerge() || c.IsSquash() {
		t.Fatalf("ordinary commit: parents=%v merge=%v squash=%v", c.Parents, c.IsMerge(), c.IsSquash())
	}

	// A merge commit has two parents.
	mustGit(t, root, "checkout", "-q", "-b", "feature")
	commit("feature", "feature work")
	mustGit(t, root, "checkout", "-q", "main")
	commit("mainline", "mainline work")
	mustGit(t, root, "merge", "-q", "--no-ff", "--no-edit", "feature")
	if c := head(); len(c.Parents) != 2 || !c.IsMerge() || c.IsSquash() {
		t.Fatalf("merge commit: parents=%v merge=%v squash=%v", c.Parents, c.IsMerge(), c.IsSquash())
	}

	// A squash is a single-parent commit; only git's default subject gives it away.
	mustGit(t, root, "checkout", "-q", "-b", "feature2")
	commit("feature2", "more feature work")
	mustGit(t, root, "checkout", "-q", "main")
	mustGit(t, root, "merge", "-q", "--squash", "feature2")
	mustGit(t, root, "commit", "-q", "--no-edit")
	if c := head(); len(c.Parents) != 1 || c.IsMerge() || !c.IsSquash() {
		t.Fatalf("squash commit: parents=%v merge=%v squash=%v\n%s", c.Parents, c.IsMerge(), c.IsSquash(), c.Message)
	}
	// A rewritten squash message reads as the ordinary commit it is, by design.
	mustGit(t, root, "commit", "-q", "--amend", "-m", "feat: squashed feature2")
	if c := head(); c.IsSquash() {
		t.Fatalf("a rewritten squash message must read as an ordinary commit: %q", c.Message)
	}
}

// RemoteURLFS has to agree with `git config --get remote.origin.url` after the
// same normalisation, including from a linked worktree, whose own git dir holds a
// `commondir` pointer rather than a config.
func TestRemoteURLFSMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "") // registers the restore
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	mustGit(t, root, "init", "-q", "-b", "main")
	gitDir := filepath.Join(root, ".git")
	if got := RemoteURLFS(gitDir); got != "" {
		t.Fatalf("no origin yet, got %q", got)
	}
	mustGit(t, root, "remote", "add", "origin", "https://dawson:ghp_secret@github.com/miradorlabs/terma-cli.git")
	want := NormalizeRemote(mustGit(t, root, "config", "--get", "remote.origin.url"))
	if want != "https://github.com/miradorlabs/terma-cli" {
		t.Fatalf("git-backed value = %q", want)
	}
	if got := RemoteURLFS(gitDir); got != want {
		t.Fatalf("RemoteURLFS = %q, want %q", got, want)
	}
	// The file is read on every call, so a changed remote is seen at once.
	mustGit(t, root, "remote", "set-url", "origin", "git@github.com:miradorlabs/other.git")
	if got, want := RemoteURLFS(gitDir), "https://github.com/miradorlabs/other"; got != want {
		t.Fatalf("after set-url: %q, want %q", got, want)
	}

	mustGit(t, root, "config", "user.email", "me@example.com")
	mustGit(t, root, "config", "user.name", "me")
	mustGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "x")
	mustGit(t, root, "commit", "-q", "-m", "x")
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, root, "worktree", "add", "-q", wt, "-b", "linked")
	_, wtGitDir, ok := LocateFS(wt)
	if !ok || !strings.Contains(wtGitDir, "worktrees") {
		t.Fatalf("LocateFS(worktree) = %q, %v", wtGitDir, ok)
	}
	if got, want := RemoteURLFS(wtGitDir), "https://github.com/miradorlabs/other"; got != want {
		t.Fatalf("from a linked worktree: %q, want %q", got, want)
	}
}
