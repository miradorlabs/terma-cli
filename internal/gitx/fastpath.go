package gitx

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The hot path. A git subprocess costs 10–15 ms on macOS, and prepare-commit-msg
// has a 50 ms budget for the whole hook, so the two lookups every hook needs —
// where the repository is and what its comment character is — read the filesystem
// directly and fall back to git only when the layout is one they do not understand.

// LocateFS finds the worktree root and git directory for dir without running git:
// it honours GIT_DIR / GIT_WORK_TREE (git exports them to some hooks) and otherwise
// walks up to the nearest `.git`, following a `gitdir:` file for linked worktrees
// and submodules. ok is false when nothing was found, or the layout is unusual
// enough that the caller should ask git.
func LocateFS(dir string) (root, gitDir string, ok bool) {
	if gd := os.Getenv("GIT_DIR"); gd != "" {
		wt := os.Getenv("GIT_WORK_TREE")
		if wt == "" {
			wt = dir
		}
		if gd, err := filepath.Abs(gd); err == nil {
			if wt, err := filepath.Abs(wt); err == nil && isDir(gd) {
				return filepath.Clean(wt), filepath.Clean(gd), true
			}
		}
		return "", "", false
	}
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false
	}
	for {
		candidate := filepath.Join(cur, ".git")
		info, err := os.Lstat(candidate)
		if err == nil {
			switch {
			case info.IsDir():
				return cur, candidate, true
			case info.Mode().IsRegular():
				target := readGitdirFile(candidate)
				if target == "" {
					return "", "", false
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(cur, target)
				}
				if !isDir(target) {
					return "", "", false
				}
				return cur, filepath.Clean(target), true
			default:
				return "", "", false
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", false
		}
		cur = parent
	}
}

// readGitdirFile parses a `.git` file ("gitdir: <path>").
func readGitdirFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
}

// CommonDirFS is the primary `.git` for a git directory: the directory itself, or
// the one its `commondir` file names in a linked worktree.
func CommonDirFS(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return gitDir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return filepath.Clean(target)
}

// LinkedWorktreeFS reports whether gitDir belongs to a linked worktree (`git worktree
// add`), with git's name for it (the directory under .git/worktrees) and the main
// checkout's root. mainRoot is "" for a worktree of a bare repository, which has no main
// checkout. A submodule's git directory has no commondir and is not a worktree. It reads
// one small file and runs nothing.
func LinkedWorktreeFS(gitDir string) (name, mainRoot string, ok bool) {
	if gitDir == "" {
		return "", "", false // a workspace outside Git
	}
	gitDir = filepath.Clean(gitDir)
	common := CommonDirFS(gitDir)
	if common == gitDir {
		return "", "", false
	}
	if filepath.Base(common) == ".git" {
		mainRoot = filepath.Dir(common)
	}
	return filepath.Base(gitDir), mainRoot, true
}

// CommentCharFS reads core.commentChar the way git resolves it — global config
// then the repository's — without a subprocess. Includes and conditional includes
// are not followed; a repository relying on those for commentChar (rare) gets "#",
// which only affects where a trailer block lands in a message that has comments.
func CommentCharFS(gitDir string) string {
	value := ""
	for _, path := range globalConfigPaths() {
		if v, ok := configValue(path, "core", "commentchar"); ok {
			value = v
		}
	}
	if v, ok := configValue(filepath.Join(CommonDirFS(gitDir), "config"), "core", "commentchar"); ok {
		value = v
	}
	if value == "" || value == "auto" {
		return "#"
	}
	return value
}

// RemoteURLFS is RemoteURL without the subprocess: it reads `remote.origin.url`
// from the repository's own config file (the common dir's, so a linked worktree
// sees the primary's remotes) and normalises it the same way — credentials
// stripped, ssh rewritten to https. Like CommentCharFS it does not follow
// includes, so a remote defined only through one yields "", and the caller omits
// the link rather than paying a git call to find it.
func RemoteURLFS(gitDir string) string {
	raw, _ := configValue(filepath.Join(CommonDirFS(gitDir), "config"), `remote "origin"`, "url")
	return NormalizeRemote(raw)
}

func globalConfigPaths() []string {
	var paths []string
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		if home, err := os.UserHomeDir(); err == nil {
			xdg = filepath.Join(home, ".config")
		}
	}
	if xdg != "" {
		paths = append(paths, filepath.Join(xdg, "git", "config"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".gitconfig"))
	}
	return paths
}

// configValue returns the last value of key in section from one git config file.
func configValue(path, section, key string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var (
		inSection bool
		value     string
		found     bool
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			inSection = strings.EqualFold(name, section)
			continue
		}
		if !inSection {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		value, found = unquoteConfigValue(v), true
	}
	return value, found
}

// FileStat is one path in a commit with the line delta git reported for it.
//
// Added and Deleted are the commit's delta for that file — every line in it,
// whoever wrote it. They are not a measurement of one session's output; see the
// note at the emit site in internal/hookrun.
type FileStat struct {
	Path string
	// Added and Deleted are meaningless when Binary is set.
	Added   int
	Deleted int
	// Binary is true when git printed "-" instead of counts, as it does for a file
	// it has no lines to count (binary content). The counts are then zero because
	// there is no number, not because nothing changed — never report them as 0.
	Binary bool
}

// Commit is what post-commit needs from HEAD, read in one git call.
type Commit struct {
	SHA         string
	AuthorEmail string
	Message     string
	Files       []FileStat
	// Branch is the local branch HEAD is on, or "" when detached. Read from the
	// same log call rather than a second subprocess.
	Branch string
	// Parents are the parent shas in git's order; two or more make a merge. Read
	// from the same log call, because post-commit is not told the commit's source
	// the way prepare-commit-msg is.
	Parents []string
}

// IsMerge reports whether the commit has more than one parent. Of the merges git
// makes, only one kind reaches post-commit at all: `git merge` runs post-merge, not
// post-commit, so what arrives here is a conflicted merge finished with `git commit`.
func (c Commit) IsMerge() bool { return len(c.Parents) > 1 }

// squashSubject is the first line git writes to SQUASH_MSG for `git merge --squash`.
const squashSubject = "Squashed commit of the following:"

// IsSquash reports whether the commit looks like the result of `git merge --squash`.
// It is a heuristic on the message: git unlinks SQUASH_MSG before it runs
// post-commit, so the default subject is the only trace left, and a squash whose
// message was rewritten by hand is indistinguishable from an ordinary commit —
// which, as a single-parent commit on the branch, it also is.
func (c Commit) IsSquash() bool {
	subject, _, _ := strings.Cut(c.Message, "\n")
	return strings.HasPrefix(strings.TrimSpace(subject), squashSubject)
}

// Paths is the plain file list, in git's order — what the manifests are consumed
// with, which needs nothing but the names.
func (c Commit) Paths() []string {
	if len(c.Files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// LastCommit reads HEAD's sha, author, refs, parents, full message and per-file
// line stats in a single invocation (separate calls would multiply the hook's cost).
//
// --numstat costs the same as the --name-only it replaced (both ~13.5 ms here,
// dominated by process start), so the line counts are free; so are the parents.
func LastCommit(ctx context.Context, dir string) (Commit, error) {
	out, err := run(ctx, dir, "log", "-1", "-z", "--format=%H%x1f%ae%x1f%D%x1f%P%x1f%B%x1e", "--numstat", "HEAD")
	if err != nil {
		return Commit{}, err
	}
	head, tail, _ := strings.Cut(out, "\x1e")
	parts := strings.SplitN(head, "\x1f", 5)
	if len(parts) < 5 {
		return Commit{}, errNoCommit
	}
	return Commit{
		SHA:         parts[0],
		AuthorEmail: parts[1],
		Branch:      branchFromRefs(parts[2]),
		Parents:     strings.Fields(parts[3]),
		Message:     strings.TrimRight(parts[4], "\n"),
		Files:       parseNumstat(tail),
	}, nil
}

// parseNumstat reads the `--numstat -z` block that follows the --format output.
//
// Records are NUL-terminated and hold "<added>\t<deleted>\t<path>", except for a
// rename or copy, which git splits across three records: the counts with an empty
// path, then the old path, then the new one. Splitting on NUL alone and treating
// every record as a file therefore desynchronises the whole rest of the block, so
// renames are consumed explicitly and recorded under the new path.
//
// -z means paths are raw: unquoted, and free to contain spaces, tabs or newlines.
// Nothing here trims or unquotes a path, and the counts are cut off the front so a
// tab inside a name stays part of it.
func parseNumstat(block string) []FileStat {
	// git terminates the --format record with a NUL and separates it from the diff
	// with a newline. Both belong to the framing, not to the first path.
	block = strings.TrimPrefix(block, "\x00")
	block = strings.TrimPrefix(block, "\n")
	records := strings.Split(block, "\x00")
	var out []FileStat
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		addedField, rest, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		deletedField, path, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		if path == "" {
			// Rename or copy: the next two records are the old and the new path.
			if i+2 >= len(records) {
				break
			}
			path = records[i+2]
			i += 2
		}
		if path == "" {
			continue
		}
		added, addedOK := parseCount(addedField)
		deleted, deletedOK := parseCount(deletedField)
		out = append(out, FileStat{
			Path:    path,
			Added:   added,
			Deleted: deleted,
			Binary:  !addedOK || !deletedOK,
		})
	}
	return out
}

// parseCount reads one numstat count. ok is false for git's "-" (no line counts
// for this file) and for anything else that is not a plain number.
func parseCount(field string) (int, bool) {
	n, err := strconv.Atoi(field)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// branchFromRefs pulls the local branch out of git's %D ref list
// ("HEAD -> main, origin/main"). A detached HEAD has no "HEAD ->" and yields "".
func branchFromRefs(refs string) string {
	for ref := range strings.SplitSeq(refs, ",") {
		ref = strings.TrimSpace(ref)
		if name, ok := strings.CutPrefix(ref, "HEAD -> "); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// unquoteConfigValue strips a trailing comment and surrounding quotes the way git
// reads a config value: `"..."` is taken literally up to the closing quote, an
// unquoted value ends at the first # or ;.
func unquoteConfigValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "\"") {
		rest := v[1:]
		var b strings.Builder
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				if i+1 < len(rest) {
					i++
					b.WriteByte(rest[i])
				}
			case '"':
				return b.String()
			default:
				b.WriteByte(rest[i])
			}
		}
		return b.String()
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
