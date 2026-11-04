package sourcelink

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Library reads harnesses' sources: each build's tarball once, kept on disk, and indexed
// once a process, so a night that asks about many names reads each build once.
type Library struct {
	// Dir keeps every tarball read, for the next process; with none they are kept in a
	// temporary directory until Close.
	Dir string
	// Open reads a tarball of repo at tag, as GitHub serves one; Download unless a test
	// replaces it.
	Open func(repo, tag string) (io.ReadCloser, error)

	mu    sync.Mutex
	tmp   string
	trees map[string]*tree
}

// New is a library that keeps the tarballs it reads in dir, or, with none, until Close.
func New(dir string) *Library { return &Library{Dir: dir, Open: Download} }

// DefaultDir is where the default library keeps tarballs: the user's cache, or none where
// there is none.
func DefaultDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "terma", "sourcelink")
}

var (
	defaultOnce sync.Once
	defaultLib  *Library
)

// Default is the library Find and FindText read through, keeping its tarballs in DefaultDir.
func Default() *Library {
	defaultOnce.Do(func() { defaultLib = New(DefaultDir()) })
	return defaultLib
}

// Find is Default().Find.
func Find(harness, version string, needles []string) (map[string][]Ref, error) {
	return Default().Find(harness, version, needles)
}

// FindText is Default().FindText.
func FindText(harness, version, text string) ([]Ref, error) {
	return Default().FindText(harness, version, text)
}

// Download reads a tarball of repo at tag from GitHub.
func Download(repo, tag string) (io.ReadCloser, error) {
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Get("https://codeload.github.com/" + repo + "/tar.gz/refs/tags/" + tag)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s at %s: %s", repo, tag, resp.Status)
	}
	return resp.Body, nil
}

// ErrNotPublic is what a lookup in a harness whose source is not public returns.
var ErrNotPublic = errors.New("source not public")

// Close removes the tarballs a library with no Dir kept, and forgets what it read. What
// cannot be removed is left to the system's temporary directory.
func (l *Library) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trees = nil
	if l.tmp != "" {
		_ = os.RemoveAll(l.tmp)
		l.tmp = ""
	}
}

// tree is a build's source: its tarball, and what has been read of it.
type tree struct {
	file  string
	err   error
	names *index
	text  *textIndex
}

// Scan finds needles in a build of s: each the lines that quote it or, for a field, assign
// it, and for a quoted one the lines that name a constant holding it, after those that
// quote it, each in the tree's order.
func (l *Library) Scan(s Source, version string, needles map[Needle]bool) (map[Needle][]Hit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.tree(s, version)
	if err != nil {
		return nil, err
	}
	if t.names == nil {
		lang, err := languageOf(s.Lang)
		if err != nil {
			return nil, err
		}
		if t.names, err = read(l, t, func(r io.Reader) (*index, error) { return indexNames(r, s, lang) }); err != nil {
			return nil, err
		}
	}
	return t.names.find(needles), nil
}

// Find is where a build of harness names each needle: the lines that quote it (directly, or
// by a constant that holds it) first, then those that assign it as a field, each line once.
// A needle named nowhere has no entry. The caller judges how many lines are too many to say
// anything: a name as common as "model" is in hundreds.
func (l *Library) Find(harness, version string, needles []string) (map[string][]Ref, error) {
	s, ok := Sources[harness]
	if !ok {
		return nil, fmt.Errorf("%s: %w", harness, ErrNotPublic)
	}
	set := map[Needle]bool{}
	for _, n := range needles {
		set[Needle{n, false}] = true
		set[Needle{n, true}] = true
	}
	hits, err := l.Scan(s, version, set)
	if err != nil {
		return nil, err
	}
	out := map[string][]Ref{}
	for _, n := range needles {
		type at struct {
			path string
			line int
		}
		seen := map[at]bool{}
		var refs []Ref
		for _, nd := range []Needle{{n, false}, {n, true}} {
			for _, h := range hits[nd] {
				if !seen[at{h.Path, h.Line}] {
					seen[at{h.Path, h.Line}] = true
					refs = append(refs, Ref{Path: h.Path, Line: h.Line, URL: s.Blob(version, h.Path, h.Line)})
				}
			}
		}
		if len(refs) > 0 {
			out[n] = refs
		}
	}
	return out, nil
}

// FindText is where a build of harness holds a passage of prose: a paragraph of its system
// prompt, a tool's description. Prose files (.md, .txt, templates) and the strings of its
// code are both read; a passage is matched by its distinctive lines, whitespace collapsed,
// so a prompt kept in a Markdown file, a template literal or a run of concatenated strings
// is found, and what the build fills in (a path, a date) is no obstacle. Each Ref is a run
// of lines that holds part of the passage, the run that holds most of it first, its URL
// spanning the run.
func (l *Library) FindText(harness, version, text string) ([]Ref, error) {
	s, ok := Sources[harness]
	if !ok {
		return nil, fmt.Errorf("%s: %w", harness, ErrNotPublic)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.tree(s, version)
	if err != nil {
		return nil, err
	}
	if t.text == nil {
		lang, err := languageOf(s.Lang)
		if err != nil {
			return nil, err
		}
		if t.text, err = read(l, t, func(r io.Reader) (*textIndex, error) { return indexText(r, s, lang) }); err != nil {
			return nil, err
		}
	}
	return t.text.find(s, version, text), nil
}

// read runs scan over t's tarball; a tarball kept on disk that cannot be read is removed,
// so the next process reads it again.
func read[T any](l *Library, t *tree, scan func(io.Reader) (T, error)) (T, error) {
	var zero T
	f, err := os.Open(t.file)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	out, err := scan(f)
	if err != nil {
		if l.Dir != "" {
			_ = os.Remove(t.file)
		}
		return zero, err
	}
	return out, nil
}

// tree is a build's source, its tarball read once a library. What could not be read is
// remembered too: a build is asked for once a process, not once a lookup.
func (l *Library) tree(s Source, version string) (*tree, error) {
	tag := s.TagOf(version)
	key := strings.Join([]string{s.Repo, tag, s.Lang, strings.Join(s.Dirs, ",")}, "\x00")
	if t, ok := l.trees[key]; ok {
		return t, t.err
	}
	if l.trees == nil {
		l.trees = map[string]*tree{}
	}
	t := &tree{}
	t.file, t.err = l.fetch(s.Repo, tag)
	l.trees[key] = t
	return t, t.err
}

// fetch is the path of repo's tarball at tag, read once into the library's directory.
func (l *Library) fetch(repo, tag string) (string, error) {
	dir := l.Dir
	if dir == "" {
		if l.tmp == "" {
			tmp, err := os.MkdirTemp("", "sourcelink-*")
			if err != nil {
				return "", err
			}
			l.tmp = tmp
		}
		dir = l.tmp
	}
	clean := func(s string) string { return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(s) }
	file := filepath.Join(dir, clean(repo), clean(tag)+".tar.gz")
	if l.Dir != "" {
		if _, err := os.Stat(file); err == nil {
			return file, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	open := l.Open
	if open == nil {
		open = Download
	}
	r, err := open(repo, tag)
	if err != nil {
		return "", err
	}
	defer r.Close()
	part, err := os.CreateTemp(filepath.Dir(file), "partial-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(part.Name()) }() // renamed into place, or what is left of it
	if _, err := io.Copy(part, r); err != nil {
		part.Close()
		return "", err
	}
	if err := part.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part.Name(), file); err != nil {
		return "", err
	}
	return file, nil
}
