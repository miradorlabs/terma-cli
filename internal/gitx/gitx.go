// Package gitx is the thin git surface the hooks and installers need. Everything
// shells out to git itself: reproducing index parsing or config resolution in Go
// would be faster but would also be a second implementation of git's rules, and
// the hooks' budget is met with one or two invocations (a few milliseconds each).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Timeout bounds any single git call from a hook. A wedged git (a stuck lock, a
// network filesystem) must fail the attribution, never hang the commit.
const Timeout = 2 * time.Second

// ErrNotRepo is returned for a directory git does not consider a repository.
var ErrNotRepo = errors.New("not a git repository")

// errNoCommit is returned when HEAD has no commit to read.
var errNoCommit = errors.New("no commit at HEAD")

func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not a git repository") {
			return "", ErrNotRepo
		}
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Locate resolves the worktree root and the metadata directory in one git call —
// the hooks' hot path, where every invocation counts against the budget.
func Locate(ctx context.Context, dir string) (root, gitDir string, err error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	return filepath.Clean(lines[0]), filepath.Clean(lines[1]), nil
}

// StagedFiles lists the repo-relative paths in the index that differ from HEAD
// (added, modified, renamed destinations). Slash-separated, NUL-safe.
func StagedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range strings.SplitSeq(out, "\x00") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// HeadSHA is the current commit, or "" in an unborn repository.
func HeadSHA(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// CommitMessage returns the full message of a commit.
func CommitMessage(ctx context.Context, dir, rev string) (string, error) {
	return run(ctx, dir, "log", "-1", "--format=%B", rev)
}

// CommentChar is core.commentChar, "#" by default.
func CommentChar(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "config", "--get", "core.commentChar")
	if err != nil || out == "" || out == "auto" {
		return "#"
	}
	return out
}

// ConfigGet reads one key from the effective configuration ("" when unset).
func ConfigGet(ctx context.Context, dir, key string) string {
	out, _ := run(ctx, dir, "config", "--get", key)
	return out
}

// ConfigSet writes a repository-local key.
func ConfigSet(ctx context.Context, dir, key, value string) error {
	_, err := run(ctx, dir, "config", "--local", key, value)
	return err
}

// ConfigUnset removes a repository-local key; a missing key is not an error.
func ConfigUnset(ctx context.Context, dir, key string) error {
	_, err := run(ctx, dir, "config", "--local", "--unset", key)
	if err != nil && strings.Contains(err.Error(), "exit status 5") {
		return nil
	}
	if err != nil && ConfigGet(ctx, dir, key) == "" {
		return nil
	}
	return err
}

// Relativize turns an absolute path inside root into a slash-separated
// repo-relative one; paths outside root come back empty.
func Relativize(root, path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(filepath.Clean(path))
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// NormalizeRemote turns a git remote into a browsable https URL, or "" when it
// cannot be one. Exported for testing and for callers holding a remote already.
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// scp-like syntax: git@host:owner/repo(.git)
	if !strings.Contains(raw, "://") {
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw[at+1:], ":")
		if at >= 0 && colon >= 0 {
			host := raw[at+1 : at+1+colon]
			path := raw[at+1+colon+1:]
			return "https://" + host + "/" + strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".git")
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	// Drop userinfo (may hold a token) and any ssh/git scheme.
	u.User = nil
	switch u.Scheme {
	case "http", "https", "ssh", "git":
		u.Scheme = "https"
	default:
		return ""
	}
	u.Path = strings.TrimSuffix(u.Path, ".git")
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// Git runs an arbitrary git command in dir (for doctor's scratch commits).
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, dir, args...)
}
