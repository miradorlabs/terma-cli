package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// Source links. With -source, a finding about a harness whose source is public links the
// lines of it that name the finding, at the build it was found in. A field or surface gone
// says which it is: still named in the new build's source (not sent tonight: a condition, a
// schedule, a scenario), or no longer (removed), with where the build before named it. A new
// build's headline links the comparison of the two. A build whose source cannot be read
// leaves its findings unlinked, and the digest says so: links never fail the night. Which
// harnesses' sources are public, and how each is read, is sourcelink's: its Sources.

// SourceRef is a line of a harness's source that names a finding.
type SourceRef = sourcelink.Ref

// needle and hit are what a build's source is scanned for, and where it names one.
type (
	needle = sourcelink.Needle
	hit    = sourcelink.Hit
)

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

// maxRefs bounds the lines a finding links; maxUnplaced the lines a key may be named in to
// be linked by its name alone (rank).
const (
	maxRefs     = 2
	maxUnplaced = 3
)

// openSource reads a tarball of repo at tag; tests replace it.
var openSource = sourcelink.Download

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
	gone    bool   // looked for in the build before too
	in      string // the build it went in, where not the last judged
	from    string // the build that sent it, of an addition, where not Version
	says    **SourceSays
}

func (f finding) names() []needle {
	if f.surface {
		return []needle{{Text: f.name}}
	}
	return []needle{{Text: f.name}, {Text: f.name, Field: true}}
}

func (f finding) needles() []needle {
	if f.within != "" {
		return append(f.names(), needle{Text: f.within})
	}
	return f.names()
}

// findings are what h names, each with where its note goes.
func (h *HarnessDrift) findings() []finding {
	var out []finding
	for i := range h.NewSurfaces {
		if name, ok := surfaceName(h.NewSurfaces[i].Surface); ok {
			out = append(out, finding{name: name, surface: true, from: h.NewSurfaces[i].From, says: &h.NewSurfaces[i].Source})
		}
	}
	for _, list := range []struct {
		cs   []FieldChange
		gone bool
	}{{h.Added, false}, {h.Withheld, false}, {h.Removed, true}} {
		for i := range list.cs {
			within, _ := surfaceName(list.cs[i].Surface)
			out = append(out, finding{name: list.cs[i].Key, within: within, gone: list.gone, in: list.cs[i].In, from: list.cs[i].From, says: &list.cs[i].Source})
		}
	}
	for i := range h.Unseen {
		if name, ok := surfaceName(h.Unseen[i].Surface); ok {
			out = append(out, finding{name: name, surface: true, gone: true, in: h.Unseen[i].In, says: &h.Unseen[i].Source})
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
	// A release's comparison is of the build known before it, censused tonight or not.
	for i := range d.Releases {
		r := &d.Releases[i]
		if s, ok := sourcelink.Sources[r.Harness]; ok && r.Previous != "" {
			r.Compare = s.Compare(r.Previous, r.Version)
		}
	}
	// Each build is read once, and nothing is kept past the night.
	lib := sourcelink.New("")
	lib.Open = func(repo, tag string) (io.ReadCloser, error) { return openSource(repo, tag) }
	defer lib.Close()
	for i := range d.Harnesses {
		h := &d.Harnesses[i]
		s, ok := sourcelink.Sources[h.Harness]
		if !ok {
			continue
		}
		// What is gone was judged against Since, where the builds between were partial.
		base := h.Previous
		if h.Since != "" {
			base = h.Since
		}
		if base != "" && versionLess(base, h.Version) {
			h.Compare = s.Compare(base, h.Version)
		}
		fs := h.findings()
		if len(fs) == 0 {
			continue
		}
		// A finding is looked for in a build: what is new in Version; what is gone in the build
		// it went in, and in the build judged before that one, the chain's or base.
		builds := func(f finding) (at, before string) {
			if !f.gone {
				if f.from != "" {
					return f.from, ""
				}
				return h.Version, ""
			}
			at = f.in
			if at == "" {
				at = h.lastJudged()
			}
			before = base
			if i := slices.Index(h.Chain, at); i > 0 {
				before = h.Chain[i-1]
			}
			if before == "" || !versionLess(before, at) {
				before = ""
			}
			return at, before
		}
		names := slices.Clone(known[h.Harness])
		for _, f := range fs {
			if f.within != "" {
				names = append(names, f.within)
			}
		}
		need := map[string]map[needle]bool{}
		want := func(version string, ns []needle) {
			if need[version] == nil {
				need[version] = map[needle]bool{}
				for _, name := range names {
					need[version][needle{Text: name}] = true
				}
			}
			for _, n := range ns {
				need[version][n] = true
			}
		}
		for _, f := range fs {
			at, before := builds(f)
			want(at, f.needles())
			if before != "" {
				want(before, f.needles())
			}
		}
		hits := map[string]map[needle][]hit{}
		for _, version := range slices.Sorted(maps.Keys(need)) {
			found, err := lib.Scan(s, version, need[version])
			if err != nil {
				d.SourceErrors = append(d.SourceErrors, fmt.Sprintf("%s %s: %v", h.Name, version, err))
				continue
			}
			hits[version] = found
		}
		for _, f := range fs {
			atVersion, beforeVersion := builds(f)
			hitsAt, hitsBefore := hits[atVersion], hits[beforeVersion]
			if hitsAt == nil {
				continue
			}
			at, atBeside, placed := rank(f, hitsAt, owners(hitsAt, names))
			says := &SourceSays{Version: atVersion, Gone: f.gone && beforeVersion != ""}
			if says.Gone && hitsBefore != nil {
				before, beforeBeside, beforePlaced := rank(f, hitsBefore, owners(hitsBefore, names))
				// A key gone is still in the source only where it is still beside its surface:
				// a generic key ("state") is named elsewhere whether or not its metric keeps it.
				if beforeBeside && !atBeside {
					at, placed = nil, true
				}
				if beforePlaced {
					says.Previous = beforeVersion
					says.Before, says.BeforeMore = refs(s, beforeVersion, before)
				}
			}
			// Unplaced, or with the build before unread named only away from its surface (it
			// may be gone), a finding is left unlinked rather than linked to what it is not.
			if !placed || (says.Gone && says.Previous == "" && f.within != "" && !atBeside) {
				continue
			}
			says.Refs, says.More = refs(s, atVersion, at)
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
	within := hits[needle{Text: f.within}]
	if f.within == "" || len(within) == 0 {
		return unplaced()
	}
	// A key's line is beside its surface where the surface named nearest it in the same
	// function, of all the harness is known to send, is its own: at what distance. A key at the
	// end of a function is not the next one's, and one in a function that names no surface the
	// harness is known to send (an event not yet censused) is no known surface's.
	distance := func(h hit) int {
		best, d := "", -1
		for _, o := range owned[h.Path] {
			if o.fn != h.Fn {
				continue
			}
			od := abs(o.line - h.Line)
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
		for _, h := range hits[needle{Text: name}] {
			out[h.Path] = append(out[h.Path], owner{h.Line, h.Fn, name})
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
func refs(s sourcelink.Source, version string, hits []hit) ([]SourceRef, int) {
	var out []SourceRef
	for _, h := range hits[:min(len(hits), maxRefs)] {
		out = append(out, SourceRef{Path: h.Path, Line: h.Line, URL: s.Blob(version, h.Path, h.Line)})
	}
	return out, len(hits) - len(out)
}

func abs(n int) int { return max(n, -n) }
