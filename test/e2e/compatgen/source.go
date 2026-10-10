package main

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
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
// found in (for one Gone, the new build), and Before in Previous, for one gone, where the
// build before could be read. More and BeforeMore count the lines past those listed.
type SourceSays struct {
	Version    string      `json:"version"`
	Gone       bool        `json:"gone,omitempty"`
	Refs       []SourceRef `json:"refs,omitempty"`
	More       int         `json:"more,omitempty"`
	Previous   string      `json:"previous,omitempty"`
	Before     []SourceRef `json:"before,omitempty"`
	BeforeMore int         `json:"before_more,omitempty"`
}

// maxRefs bounds the lines a finding links; maxHits the lines a scan keeps of one name, far
// past any the ranking needs, so it only bounds a pathological tree; maxUnplaced the lines a
// key may be named in to be linked by its name alone (rank).
const (
	maxRefs     = 2
	maxHits     = 10000
	maxUnplaced = 3
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
//
// known names, per harness, every surface it is known to send (knownSurfaces): a line that
// names a key belongs to the surface named nearest it, so in a file that names many (Codex
// keeps its log events in one) a key is placed by the surface that owns its line.
func linkSources(d *Drift, known map[string][]string) {
	for i := range d.Harnesses {
		h := &d.Harnesses[i]
		s, ok := sources[h.Harness]
		if !ok {
			continue
		}
		// The build before is the one what is gone was judged against (Since, where the builds
		// between were partial).
		base := h.Previous
		if h.Since != "" {
			base = h.Since
		}
		newer := base != "" && versionLess(base, h.Version)
		if newer {
			h.Compare = s.compare(base, h.Version)
		}
		fs := h.findings()
		if len(fs) == 0 {
			continue
		}
		at, before := map[needle]bool{}, map[needle]bool{}
		names := slices.Clone(known[h.Harness])
		gone := false
		for _, f := range fs {
			for _, n := range f.needles() {
				at[n] = true
				if f.gone && newer {
					before[n], gone = true, true
				}
			}
			if f.within != "" {
				names = append(names, f.within)
			}
		}
		for _, name := range names {
			at[needle{name, false}] = true
			if gone {
				before[needle{name, false}] = true
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
		hitsAt, hitsBefore := read(h.Version, at), read(base, before)
		for _, f := range fs {
			if hitsAt == nil {
				continue
			}
			at, atBeside, placed := rank(f, hitsAt, owners(hitsAt, names))
			says := &SourceSays{Version: h.Version, Gone: f.gone && newer}
			if says.Gone && hitsBefore != nil {
				before, beforeBeside, beforePlaced := rank(f, hitsBefore, owners(hitsBefore, names))
				// A key gone is still in the source only where it is still beside its surface:
				// a generic key ("state") is named elsewhere whether or not its metric keeps it.
				if beforeBeside && !atBeside {
					at, placed = nil, true
				}
				if beforePlaced {
					says.Previous = base
					says.Before, says.BeforeMore = refs(s, base, before)
				}
			}
			// Unplaced, or with the build before unread named only away from its surface (it
			// may be gone), a finding is left unlinked rather than linked to what it is not.
			if !placed || (says.Gone && says.Previous == "" && f.within != "" && !atBeside) {
				continue
			}
			says.Refs, says.More = refs(s, h.Version, at)
			*f.says = says
		}
	}
}

// rank orders the lines that name f, and says whether they sit beside its surface: a key
// whose surface the build's source names is ranked by the files that name both, nearest that
// name first; any other finding, a quoted name before an assigned one. A key named nowhere
// beside its surface is placed only if the source names it in at most maxUnplaced lines:
// a name as common as "model" placed by its name alone would link lines that have nothing to
// do with the finding.
func rank(f finding, hits map[needle][]hit, owned map[string][]owner) (all []hit, beside, placed bool) {
	for _, n := range f.names() {
		for _, h := range hits[n] {
			if !slices.Contains(all, h) {
				all = append(all, h)
			}
		}
	}
	unplaced := func() ([]hit, bool, bool) {
		if !f.surface && len(all) > maxUnplaced {
			return nil, false, false
		}
		return all, false, true
	}
	within := hits[needle{f.within, false}]
	if f.within == "" || len(within) == 0 {
		return unplaced()
	}
	// A key's line is beside its surface where the surface named nearest it in the same
	// function, of all the harness is known to send, is its own: at what distance. A key at the
	// end of a function is not the next one's, and one in a function that names no surface the
	// harness is known to send (an event not yet censused) is no known surface's.
	distance := func(h hit) int {
		best, d := "", -1
		for _, o := range owned[h.path] {
			if o.fn != h.fn {
				continue
			}
			od := abs(o.line - h.line)
			if d < 0 || od < d || (od == d && o.name == f.within) {
				best, d = o.name, od
			}
		}
		if best != f.within {
			return -1
		}
		return d
	}
	var near []hit
	for _, h := range all {
		if distance(h) >= 0 {
			near = append(near, h)
		}
	}
	if len(near) == 0 {
		return unplaced()
	}
	slices.SortStableFunc(near, func(a, b hit) int { return distance(a) - distance(b) })
	return near, true, true
}

// owner is a line that names a surface.
type owner struct {
	line, fn int
	name     string
}

// owners are, per file, the lines that name each of names.
func owners(hits map[needle][]hit, names []string) map[string][]owner {
	out := map[string][]owner{}
	for _, name := range names {
		for _, h := range hits[needle{name, false}] {
			out[h.path] = append(out[h.path], owner{h.line, h.fn, name})
		}
	}
	return out
}

// knownSurfaces names, per harness, the surfaces the catalog and the night's census know it
// to send, by the names its source gives them.
func knownSurfaces(cat Catalog, rows []e2e.FieldRow) map[string][]string {
	set := map[string]map[string]bool{}
	add := func(harness, surface string) {
		if name, ok := surfaceName(surface); ok {
			if set[harness] == nil {
				set[harness] = map[string]bool{}
			}
			set[harness][name] = true
		}
	}
	for _, f := range cat.Fields {
		add(f.Harness, f.Surface)
	}
	for _, r := range rows {
		add(r.Harness, r.Surface)
	}
	out := map[string][]string{}
	for h, names := range set {
		out[h] = slices.Sorted(maps.Keys(names))
	}
	return out
}

// refs links at most maxRefs of hits in version, and counts the rest.
func refs(s source, version string, hits []hit) ([]SourceRef, int) {
	var out []SourceRef
	for _, h := range hits[:min(len(hits), maxRefs)] {
		out = append(out, SourceRef{Path: h.path, Line: h.line, URL: s.blob(version, h.path, h.line)})
	}
	return out, len(hits) - len(out)
}
