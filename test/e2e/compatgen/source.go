package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Source links. With -source, a finding about a harness whose source is public links the
// lines of it that name the finding, at the build it was found in. A field or surface gone
// says which it is: still named in the new build's source (not sent tonight: a condition, a
// schedule, a scenario), or no longer (removed), with where the build before named it. A new
// build's headline links the comparison of the two. A build whose source cannot be read
// leaves its findings unlinked, and the digest says so: links never fail the night.

// source is where a harness's source is public.
type source struct {
	repo string // on GitHub, owner/name
	tag  string // a build's tag, with %s for its version
	dir  string // the part of the tree that holds the code that names its fields
	ext  string // the files that do
}

var sources = map[string]source{
	"codex": {repo: "openai/codex", tag: "rust-v%s", dir: "codex-rs/", ext: ".rs"},
}

func (s source) tagOf(version string) string { return fmt.Sprintf(s.tag, version) }

func (s source) blob(version, file string, line int) string {
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%d", s.repo, s.tagOf(version), file, line)
}

func (s source) compare(from, to string) string {
	return fmt.Sprintf("https://github.com/%s/compare/%s...%s", s.repo, s.tagOf(from), s.tagOf(to))
}

// SourceRef is a line of a harness's source that names a finding.
type SourceRef struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	URL  string `json:"url"`
}

// SourceSays is where a harness's source names a finding: Refs in Version, the build it was
// found in (for one gone, the new build), and Before in Previous, for one gone. More and
// BeforeMore count the lines past those listed.
type SourceSays struct {
	Version    string      `json:"version"`
	Refs       []SourceRef `json:"refs,omitempty"`
	More       int         `json:"more,omitempty"`
	Previous   string      `json:"previous,omitempty"`
	Before     []SourceRef `json:"before,omitempty"`
	BeforeMore int         `json:"before_more,omitempty"`
}

// maxRefs bounds the lines a finding links; maxHits the lines a scan keeps of one name.
const (
	maxRefs = 2
	maxHits = 50
)

// openSource reads a tarball of repo at tag; tests replace it.
var openSource = func(repo, tag string) (io.ReadCloser, error) {
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

// needle is a name to find: as a quoted string, or as a name assigned to, the way a tracing
// macro names a field ("codex.turn.phase = ...").
type needle struct {
	text  string
	field bool
}

type hit struct {
	path string
	line int
}

var assignedName = regexp.MustCompile(`(?:^|[\s,({])([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*=[^=>]`)

// scan reads a tarball of s's tree, as GitHub serves one (every path under one folder), and
// finds where each needle is named, outside tests and comments.
func scan(r io.Reader, s source, needles map[needle]bool) (map[needle][]hit, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	out := map[needle][]hit{}
	add := func(n needle, h hit) {
		if needles[n] && len(out[n]) < maxHits {
			out[n] = append(out[n], h)
		}
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		_, file, _ := strings.Cut(h.Name, "/")
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(file, s.dir) || !strings.HasSuffix(file, s.ext) || testFile(file) {
			continue
		}
		sc := bufio.NewScanner(tr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			parts := strings.Split(line, `"`)
			for i := 1; i < len(parts)-1; i += 2 {
				add(needle{parts[i], false}, hit{file, n})
			}
			if strings.Contains(line, "=") {
				for _, m := range assignedName.FindAllStringSubmatchIndex(line, -1) {
					before := strings.Fields(line[:m[2]])
					if len(before) > 0 && slices.Contains([]string{"let", "mut", "const", "static", "type"}, before[len(before)-1]) {
						continue
					}
					add(needle{line[m[2]:m[3]], true}, hit{file, n})
				}
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
}

func abs(n int) int { return max(n, -n) }

func testFile(file string) bool {
	base := path.Base(file)
	return strings.Contains(file, "/tests/") || strings.Contains(file, "/benches/") || base == "tests.rs" ||
		strings.HasSuffix(base, "_test.rs") || strings.HasSuffix(base, "_tests.rs")
}

// surfaceName is the name a harness's source gives a surface: a log event's, a metric's, a
// span's or a span event's; none for the resource, the scope, or a name terma made up.
func surfaceName(surface string) (string, bool) {
	signal, rest, _ := strings.Cut(surface, "/")
	switch signal {
	case "logs", "metrics":
		rest = strings.TrimSuffix(rest, "/exemplars")
	case "traces":
		if _, event, ok := strings.Cut(rest, "/events/"); ok {
			rest = event
		}
		rest = strings.TrimSuffix(rest, "/links")
	default:
		return "", false
	}
	if rest == "" || strings.ContainsAny(rest, "{(") {
		return "", false
	}
	return rest, true
}

// finding is one thing a harness's drift names, and where its source note goes. A key's
// within is its surface's name, where the source names it: a key is linked where it sits
// beside its surface, so a generic one ("state") leads to its metric, not to every "state".
type finding struct {
	name    string
	surface bool // a surface's name, not a key's
	within  string
	gone    bool // looked for in the build before too
	says    **SourceSays
}

func (f finding) names() []needle {
	if f.surface {
		return []needle{{f.name, false}}
	}
	return []needle{{f.name, false}, {f.name, true}}
}

func (f finding) needles() []needle {
	if f.within != "" {
		return append(f.names(), needle{f.within, false})
	}
	return f.names()
}

// findings are what h names, each with where its note goes.
func (h *HarnessDrift) findings() []finding {
	var out []finding
	for i := range h.NewSurfaces {
		if name, ok := surfaceName(h.NewSurfaces[i].Surface); ok {
			out = append(out, finding{name: name, surface: true, says: &h.NewSurfaces[i].Source})
		}
	}
	for _, list := range []struct {
		cs   []FieldChange
		gone bool
	}{{h.Added, false}, {h.Withheld, false}, {h.Removed, true}} {
		for i := range list.cs {
			within, _ := surfaceName(list.cs[i].Surface)
			out = append(out, finding{name: list.cs[i].Key, within: within, gone: list.gone, says: &list.cs[i].Source})
		}
	}
	for i := range h.Unseen {
		if name, ok := surfaceName(h.Unseen[i].Surface); ok {
			out = append(out, finding{name: name, surface: true, gone: true, says: &h.Unseen[i].Source})
		}
	}
	return out
}

// linkSources links d's findings to the source of each harness whose source is public.
func linkSources(d *Drift) {
	for i := range d.Harnesses {
		h := &d.Harnesses[i]
		s, ok := sources[h.Harness]
		if !ok {
			continue
		}
		newer := h.Previous != "" && versionLess(h.Previous, h.Version)
		if newer {
			h.Compare = s.compare(h.Previous, h.Version)
		}
		fs := h.findings()
		if len(fs) == 0 {
			continue
		}
		at, before := map[needle]bool{}, map[needle]bool{}
		for _, f := range fs {
			for _, n := range f.needles() {
				at[n] = true
				if f.gone && newer {
					before[n] = true
				}
			}
		}
		read := func(version string, needles map[needle]bool) map[needle][]hit {
			if len(needles) == 0 {
				return nil
			}
			hits, err := scanBuild(s, version, needles)
			if err != nil {
				d.SourceErrors = append(d.SourceErrors, fmt.Sprintf("%s %s: %v", h.Name, version, err))
				return nil
			}
			return hits
		}
		hitsAt, hitsBefore := read(h.Version, at), read(h.Previous, before)
		for _, f := range fs {
			if hitsAt == nil || (f.gone && newer && hitsBefore == nil) {
				continue
			}
			says := &SourceSays{Version: h.Version}
			says.Refs, says.More = refsOf(s, h.Version, f, hitsAt)
			if f.gone && newer {
				says.Previous = h.Previous
				says.Before, says.BeforeMore = refsOf(s, h.Previous, f, hitsBefore)
			}
			*f.says = says
		}
	}
}

func scanBuild(s source, version string, needles map[needle]bool) (map[needle][]hit, error) {
	r, err := openSource(s.repo, s.tagOf(version))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return scan(r, s, needles)
}

// refsOf links the lines that name f in version, at most maxRefs, and counts the rest. A key
// whose surface the source names is linked only in the files that name both, nearest that
// name first; any other finding, a quoted name before an assigned one.
func refsOf(s source, version string, f finding, hits map[needle][]hit) ([]SourceRef, int) {
	var all []hit
	for _, n := range f.names() {
		for _, h := range hits[n] {
			if !slices.Contains(all, h) {
				all = append(all, h)
			}
		}
	}
	if within := hits[needle{f.within, false}]; f.within != "" && len(within) > 0 {
		distance := func(h hit) int {
			d := -1
			for _, w := range within {
				if w.path == h.path && (d < 0 || abs(w.line-h.line) < d) {
					d = abs(w.line - h.line)
				}
			}
			return d
		}
		var beside []hit
		for _, h := range all {
			if distance(h) >= 0 {
				beside = append(beside, h)
			}
		}
		if len(beside) > 0 {
			slices.SortStableFunc(beside, func(a, b hit) int { return distance(a) - distance(b) })
			all = beside
		}
	}
	var refs []SourceRef
	for _, h := range all[:min(len(all), maxRefs)] {
		refs = append(refs, SourceRef{Path: h.path, Line: h.line, URL: s.blob(version, h.path, h.line)})
	}
	return refs, len(all) - len(refs)
}
