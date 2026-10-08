package gitx

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The *FS functions read the filesystem instead of running git: a git subprocess
// costs 10–15 ms on macOS, and prepare-commit-msg has a 50 ms budget.

// LocateFS finds the worktree root and git directory for dir without running git; ok
// is false when nothing was found or the layout is unusual enough to ask git.
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

// LinkedWorktreeFS reports whether gitDir is a linked worktree's, with git's name for it
// and the main checkout's root ("" for a bare repository's worktree).
func LinkedWorktreeFS(gitDir string) (name, mainRoot string, ok bool) {
	if gitDir == "" {
		return "", "", false
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

// CommentCharFS reads core.commentChar from global then repository config; includes
// are not followed, so a value set only through one reads as "#".
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

// RemoteURLFS reads remote.origin.url from the common dir's config through
// NormalizeRemote; includes are not followed. "" outside git.
func RemoteURLFS(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	raw, _ := configValue(filepath.Join(CommonDirFS(gitDir), "config"), `remote "origin"`, "url")
	return NormalizeRemote(raw)
}

// GlobalHooksPathFS reads core.hooksPath from git's global config, without running git;
// includes are not followed.
func GlobalHooksPathFS() string {
	value := ""
	for _, path := range globalConfigPaths() {
		if v, ok := configValue(path, "core", "hookspath"); ok {
			value = v
		}
	}
	return value
}

// HooksPathFS reads core.hooksPath as the checkout's own config sets it, without running
// git: the shared local value, and the worktree-scoped one, which outranks it.
func HooksPathFS(gitDir string) (local, worktree string) {
	common := CommonDirFS(gitDir)
	local, _ = configValue(filepath.Join(common, "config"), "core", "hookspath")
	if v, _ := configValue(filepath.Join(common, "config"), "extensions", "worktreeconfig"); strings.EqualFold(v, "true") {
		worktree, _ = configValue(filepath.Join(gitDir, "config.worktree"), "core", "hookspath")
	}
	return local, worktree
}

// RepositoryFS is origin's RepositoryID, read from the main repository's config so a
// linked worktree shares it; "" outside git.
func RepositoryFS(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	raw, _ := configValue(filepath.Join(CommonDirFS(gitDir), "config"), `remote "origin"`, "url")
	return RepositoryID(raw)
}

func globalConfigPaths() []string {
	// GIT_CONFIG_GLOBAL replaces both of git's own global files.
	if p := os.Getenv("GIT_CONFIG_GLOBAL"); p != "" {
		return []string{p}
	}
	// Git reads $HOME, Git for Windows included; os.UserHomeDir is %USERPROFILE% there.
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	var paths []string
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" && home != "" {
		xdg = filepath.Join(home, ".config")
	}
	if xdg != "" {
		paths = append(paths, filepath.Join(xdg, "git", "config"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".gitconfig"))
	}
	return paths
}

// configValue returns the last value of key in section from one git config file.
func configValue(path, section, key string) (string, bool) {
	values := configValues(path, section, key)
	if len(values) == 0 {
		return "", false
	}
	return values[len(values)-1], true
}

// configValues returns every value of key in section from one git config file, in order.
func configValues(path, section, key string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var (
		inSection bool
		values    []string
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
		values = append(values, unquoteConfigValue(v))
	}
	return values
}

// FileStat is one path in a commit with git's line delta for it, whoever wrote the lines.
type FileStat struct {
	Path    string
	Added   int
	Deleted int
	// Binary means git printed "-": the counts are absent, never report them as 0.
	Binary bool
}

// Commit is what post-commit needs from HEAD, read in one git call.
type Commit struct {
	SHA         string
	AuthorEmail string
	Message     string
	Files       []FileStat
	// Branch is the local branch HEAD is on, or "" when detached.
	Branch string
	// Parents are the parent shas in git's order.
	Parents []string
}

// IsMerge reports whether the commit has more than one parent; in post-commit that is
// a conflicted merge finished with `git commit`.
func (c Commit) IsMerge() bool { return len(c.Parents) > 1 }

// squashSubject is the first line git writes to SQUASH_MSG for `git merge --squash`.
const squashSubject = "Squashed commit of the following:"

// IsSquash reports whether the message keeps `git merge --squash`'s default subject,
// the only trace left once git unlinks SQUASH_MSG.
func (c Commit) IsSquash() bool {
	subject, _, _ := strings.Cut(c.Message, "\n")
	return strings.HasPrefix(strings.TrimSpace(subject), squashSubject)
}

// Paths is the plain file list, in git's order.
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

// LastCommit reads HEAD's sha, author, refs, parents, message and numstat in one git
// call (~13.5 ms, nearly all process start).
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

// parseNumstat reads the `--numstat -z` block. A rename spans three records (counts
// with an empty path, old path, new path) and is recorded under the new path; paths
// are raw, so nothing trims them.
func parseNumstat(block string) []FileStat {
	// The NUL and newline after the --format record are framing, not the first path.
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

// parseCount reads one numstat count; ok is false for git's "-" or anything not a number.
func parseCount(field string) (int, bool) {
	n, err := strconv.Atoi(field)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// branchFromRefs pulls the local branch out of git's %D list ("HEAD -> main, origin/main").
func branchFromRefs(refs string) string {
	for ref := range strings.SplitSeq(refs, ",") {
		ref = strings.TrimSpace(ref)
		if name, ok := strings.CutPrefix(ref, "HEAD -> "); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// unquoteConfigValue strips quotes and a trailing comment the way git reads a value.
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
