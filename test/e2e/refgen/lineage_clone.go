package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// Local copies of public harnesses' sources: a git clone of each repository, kept between
// runs, that reads a release's tree without fetching its tarball and holds the history a
// lineage lookup searches. Each is <root>/<owner>-<name> (openai-codex), root being
// -source-clones, else $TERMA_SOURCE_CLONES, else $XDG_CACHE_HOME/terma-harness-reference
// (~/.cache/terma-harness-reference). A run that looks lineage sources up clones a repository
// there once (blobless: a tree's files are fetched the first time something reads them) and
// fetches its new tags on later runs; any run that finds a clone there reads releases from
// it, and falls back to the network for a tag it does not have.

// envSourceClones overrides where clones are kept.
const envSourceClones = "TERMA_SOURCE_CLONES"

// defaultCloneRoot is where clones are kept when no flag says.
func defaultCloneRoot() string {
	if dir := os.Getenv(envSourceClones); dir != "" {
		return dir
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "terma-harness-reference")
}

// sourceClone is a local clone of a harness's repository.
type sourceClone struct {
	dir  string
	repo string // owner/name on GitHub
}

// clonePath is where a repository's clone is kept under root.
func clonePath(root, repo string) string {
	return filepath.Join(root, strings.ReplaceAll(repo, "/", "-"))
}

// openClone is the clone of repo kept under root, nil where there is none.
func openClone(root, repo string) *sourceClone {
	if root == "" {
		return nil
	}
	c := &sourceClone{dir: clonePath(root, repo), repo: repo}
	if _, err := os.Stat(filepath.Join(c.dir, ".git")); err != nil {
		return nil
	}
	return c
}

// ensureClone clones repo under root, or fetches the tags the clone does not have yet.
func ensureClone(root, repo string) (*sourceClone, error) {
	if root == "" {
		return nil, errors.New("no folder for source clones: set " + envSourceClones)
	}
	if c := openClone(root, repo); c != nil {
		if _, err := c.git(nil, "fetch", "--quiet", "--tags", "--force", "origin"); err != nil {
			return nil, err
		}
		return c, nil
	}
	dir := clonePath(root, repo)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "clone", "--quiet", "--filter=blob:none", "--no-checkout", "https://github.com/"+repo+".git", dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("clone %s: %w", repo, err)
	}
	return &sourceClone{dir: dir, repo: repo}, nil
}

func (c *sourceClone) git(stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", c.dir}, args...)...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// hasRev reports whether the clone has a commit by that name.
func (c *sourceClone) hasRev(rev string) bool {
	_, err := c.git(nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}

// tarball is the tree at rev as GitHub serves a tag's: gzipped, under one folder.
func (c *sourceClone) tarball(rev string) (io.ReadCloser, error) {
	var data []byte
	err := retry(func() (err error) {
		data, err = c.git(nil, "archive", "--format=tar.gz", "--prefix=src/", rev)
		return err
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// retry runs f again where it fails once: a blobless clone fetches the files a tree needs the
// first time it is read, and a fetch can fail.
func retry(f func() error) error {
	if err := f(); err == nil {
		return nil
	}
	return f()
}

// files are the files at rev under the paths specs name (all, with none) that want takes, by
// their path from the top.
func (c *sourceClone) files(rev string, specs []string, want func(string) bool) (out map[string][]byte, err error) {
	err = retry(func() (err error) {
		out, err = c.readFiles(rev, specs, want)
		return err
	})
	return out, err
}

func (c *sourceClone) readFiles(rev string, specs []string, want func(string) bool) (map[string][]byte, error) {
	args := []string{"-C", c.dir, "archive", "--format=tar", rev}
	if len(specs) > 0 {
		args = append(append(args, "--"), specs...)
	}
	cmd := exec.Command("git", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	tr := tar.NewReader(stdout)
	var readErr error
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		if h.Typeflag != tar.TypeReg || !want(h.Name) {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			readErr = err
			break
		}
		out[h.Name] = b
	}
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git archive %s: %w: %s", rev, err, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return nil, readErr
	}
	return out, nil
}

// releases are the versions of the clone's release tags as s names them, oldest first: no
// prerelease.
func (c *sourceClone) releases(s sourcelink.Source) ([]string, error) {
	out, err := c.git(nil, "tag", "--list")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile("^" + strings.Replace(regexp.QuoteMeta(s.Tag), "%s", `(\d+(?:\.\d+)*)`, 1) + "$")
	var vs []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			vs = append(vs, m[1])
		}
	}
	slices.SortFunc(vs, func(a, b string) int {
		switch {
		case versionLess(a, b):
			return -1
		case versionLess(b, a):
			return 1
		}
		return 0
	})
	return slices.Compact(vs), nil
}

// shaRE is a commit's full name.
var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// useClones makes lib read a release from its repository's clone under root where there is
// one that has its tag (or, for a lookup's commit, the commit its version names), and from the
// network otherwise.
func useClones(lib *sourcelink.Library, root string) {
	clones := map[string]*sourceClone{}
	for _, s := range sourcelink.Sources {
		if c := openClone(root, s.Repo); c != nil {
			clones[s.Repo] = c
		}
	}
	if len(clones) == 0 {
		return
	}
	lib.Open = cloneOpener(clones)
}

func cloneOpener(clones map[string]*sourceClone) func(repo, tag string) (io.ReadCloser, error) {
	return func(repo, tag string) (io.ReadCloser, error) {
		if c := clones[repo]; c != nil {
			if c.hasRev("refs/tags/" + tag) {
				return c.tarball("refs/tags/" + tag)
			}
			for _, s := range sourcelink.Sources {
				if s.Repo != repo {
					continue
				}
				if rev, ok := strings.CutPrefix(tag, strings.Replace(s.Tag, "%s", "", 1)); ok && shaRE.MatchString(rev) && c.hasRev(rev) {
					return c.tarball(rev)
				}
			}
		}
		return sourcelink.Download(repo, tag)
	}
}
