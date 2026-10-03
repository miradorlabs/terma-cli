// Package gitx is the thin git surface hooks and installers need: bounded git
// subprocesses, plus filesystem reads for the hook hot path.
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

// Timeout bounds any single git call from a hook, so a wedged git never hangs a commit.
const Timeout = 2 * time.Second

// ErrNotRepo is returned for a directory git does not consider a repository.
var ErrNotRepo = errors.New("not a git repository")

var errNoCommit = errors.New("no commit at HEAD")

func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runWithin(ctx, Timeout, dir, args...)
}

func runWithin(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// A git killed at its deadline reports only "signal: killed".
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s: did not finish within %s", subcommand(args), timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not a git repository") {
			return "", ErrNotRepo
		}
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", subcommand(args), err)
		}
		return "", fmt.Errorf("git %s: %s", subcommand(args), msg)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// subcommand names the git command in args, past leading global options such as -c.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "-C":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// Locate resolves the worktree root and the git directory in one git call.
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

// StagedFiles lists the repo-relative paths added, modified or renamed in the index.
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

// NormalizeRemote turns a git remote into a browsable https URL, or "" when it cannot be one.
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// scp-like syntax: git@host:owner/repo(.git); a local path, C:\repo included, is none.
	if !strings.Contains(raw, "://") {
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw[at+1:], ":")
		if at >= 0 && colon >= 0 && !strings.ContainsAny(raw[:at+1+colon], `/\`) {
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
	// Userinfo may hold a token.
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

// Git runs an arbitrary git command in dir, bounded by Timeout.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, dir, args...)
}

// GitWithin is Git with its own bound, for work such as a full checkout that outlasts Timeout.
func GitWithin(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	return runWithin(ctx, timeout, dir, args...)
}
