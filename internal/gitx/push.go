package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ZeroOID is the commit id git writes for a ref that does not exist.
func ZeroOID(sha string) bool { return sha != "" && strings.Trim(sha, "0") == "" }

// RefFS reads ref's commit id from the common git directory without running git: the loose
// file, then packed-refs. sha is "" for a ref that does not exist; readable is false where
// refs cannot be read from files, as in a reftable repository.
func RefFS(gitDir, ref string) (sha string, readable bool) {
	refs, readable := RefsFS(gitDir, ref)
	return refs[ref], readable
}

// RefsFS reads every ref named prefix, or under it when prefix ends in "/", with its commit
// id, as RefFS reads one.
func RefsFS(gitDir, prefix string) (refs map[string]string, readable bool) {
	common := CommonDirFS(gitDir)
	if isDir(filepath.Join(common, "reftable")) {
		return nil, false
	}
	refs = map[string]string{}
	match := func(name string) bool {
		if strings.HasSuffix(prefix, "/") {
			return strings.HasPrefix(name, prefix)
		}
		return name == prefix
	}
	if data, err := os.ReadFile(filepath.Join(common, "packed-refs")); err == nil {
		for line := range strings.Lines(string(data)) {
			sha, name, ok := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
			if ok && ValidOID(sha) && match(name) {
				refs[name] = sha
			}
		}
	}
	// A loose ref is newer than its packed copy.
	root := filepath.Join(common, filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(common, path)
		if err != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if !match(name) {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			if sha := strings.TrimSpace(string(data)); ValidOID(sha) {
				refs[name] = sha
			}
		}
		return nil
	})
	return refs, true
}

// ValidOID reports whether s is a full commit id, SHA-1 or SHA-256.
func ValidOID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// RemoteFS reports whether name is a remote configured with a URL.
func RemoteFS(gitDir, name string) bool {
	_, ok := configValue(filepath.Join(CommonDirFS(gitDir), "config"), `remote "`+name+`"`, "url")
	return ok
}

// TrackingRefFS is the remote-tracking ref git updates when a push of remoteRef to the
// configured remote succeeds, under git's default fetch mapping. ok is false when that
// branch would not show the push: no such remote, a push URL apart from the fetch URL,
// another fetch mapping, or a ref outside refs/heads.
func TrackingRefFS(gitDir, remote, remoteRef string) (ref string, ok bool) {
	branch, isBranch := strings.CutPrefix(remoteRef, "refs/heads/")
	if remote == "" || !isBranch || branch == "" {
		return "", false
	}
	config := filepath.Join(CommonDirFS(gitDir), "config")
	section := `remote "` + remote + `"`
	url, hasURL := configValue(config, section, "url")
	if !hasURL {
		return "", false
	}
	// A fetch from one repository says nothing about a push to another; the same one
	// fetched over HTTPS and pushed over SSH is still the one.
	for _, push := range configValues(config, section, "pushurl") {
		if push != url && (RepositoryID(push) == "" || RepositoryID(push) != RepositoryID(url)) {
			return "", false
		}
	}
	fetch := configValues(config, section, "fetch")
	if len(fetch) != 1 || strings.TrimPrefix(fetch[0], "+") != "refs/heads/*:refs/remotes/"+remote+"/*" {
		return "", false
	}
	return "refs/remotes/" + remote + "/" + branch, true
}

// PushedCommit is one commit a push sent.
type PushedCommit struct {
	SHA string
	// Trailers are its Agent-Session-Id and Agent-Tool trailer lines, in their order, which
	// pairs each tool with its session.
	Trailers string
}

// PushWalkTimeout bounds the history walk behind a push. It runs detached, after the push.
const PushWalkTimeout = 30 * time.Second

// Commits lists the commits reachable from tip and from none of exclude, newest first, at
// most limit, with each one's Agent-Session-Id and Agent-Tool trailers. It never fetches: a
// partial clone's missing object is an error.
func Commits(ctx context.Context, dir, tip string, exclude []string, limit int) ([]PushedCommit, error) {
	ctx, cancel := context.WithTimeout(ctx, PushWalkTimeout)
	defer cancel()
	var revs strings.Builder
	revs.WriteString(tip + "\n")
	for _, e := range exclude {
		revs.WriteString("^" + e + "\n")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "--stdin", "--no-show-signature",
		fmt.Sprintf("--max-count=%d", limit),
		"--format=%H%x1f%(trailers:key=Agent-Session-Id,key=Agent-Tool,unfold)%x1e")
	cmd.Env = detachedEnv()
	cmd.Stdin = strings.NewReader(revs.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out []PushedCommit
	for record := range strings.SplitSeq(stdout.String(), "\x1e") {
		sha, trailers, _ := strings.Cut(strings.TrimLeft(record, "\n"), "\x1f")
		if !ValidOID(sha) {
			continue
		}
		out = append(out, PushedCommit{SHA: sha, Trailers: trailers})
	}
	return out, nil
}

// HasCommit reports whether sha is a commit in the local repository, without fetching it.
func HasCommit(ctx context.Context, dir, sha string) bool {
	return gitStatus(ctx, dir, "cat-file", "-e", sha+"^{commit}") == nil
}

// IsAncestor reports whether a is an ancestor of b; ok is false when git could not tell.
func IsAncestor(ctx context.Context, dir, a, b string) (ancestor, ok bool) {
	err := gitStatus(ctx, dir, "merge-base", "--is-ancestor", a, b)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, true
	}
	return false, false
}

// gitStatus runs git for its exit status alone, never fetching.
func gitStatus(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, PushWalkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = detachedEnv()
	return cmd.Run()
}

// repositoryEnv are the variables git sets for a hook to name its repository. A detached
// terma inherits them from whichever hook started it, which may be another repository's.
var repositoryEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE"}

// detachedEnv is the environment for git run on a recorded repository: -C alone names it,
// and nothing is fetched.
func detachedEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(repositoryEnv, name)
	})
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
}
