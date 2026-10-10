package main

import (
	"maps"
	"slices"

	"github.com/miradorlabs/terma-cli/e2e"
)

// Judging one harness's night (fieldDrift): what is new in it (additions), which of its
// builds are judged for what is gone (judgedBuilds), against what (chooseBaseline), and what
// each no longer sends (judgeChain).

// night is one harness's census tonight.
type night struct {
	harness string
	rows    []e2e.FieldRow
	ran     censusRuns
	newest  string
}

func (n night) partial(version string) bool { return n.ran.failed[n.harness+"\x00"+version] }

// sending is what a build sent: its keys, by surface and key, and its surfaces.
type sending struct {
	keys     map[string]FieldChange
	surfaces []string
}

func fieldID(surface, key string) string { return surface + "\x00" + key }

// sends is what a build sent tonight, as its rows say.
func (n night) sends(version string) sending {
	keys, surfaces := map[string]FieldChange{}, map[string]bool{}
	for _, r := range n.rows {
		if r.Version == version {
			keys[fieldID(r.Surface, r.Key)] = FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
			surfaces[r.Surface] = true
		}
	}
	return sending{keys, slices.Sorted(maps.Keys(surfaces))}
}

// seen is every key tonight, the newest build's row first: whatever the catalog has not seen
// goes into it tonight, from a re-run's error path or a build's first whole census as much as
// from a new build, and would never be new again.
func (n night) seen() map[string]e2e.FieldRow {
	builds := slices.Collect(maps.Keys(rowBuilds(n.rows)))
	sortVersionsDesc(builds)
	out := map[string]e2e.FieldRow{}
	for _, v := range builds {
		for _, r := range n.rows {
			if _, ok := out[fieldID(r.Surface, r.Key)]; !ok && r.Version == v {
				out[fieldID(r.Surface, r.Key)] = r
			}
		}
	}
	return out
}

// catalogIndex is what the catalog knows of every harness, before tonight.
type catalogIndex struct {
	known    map[string]FieldEntry
	surfaces map[string]map[string]bool // harness → surfaces
	keys     map[string]map[string]bool // harness → keys, on any surface
}

func indexCatalog(cat Catalog) catalogIndex {
	idx := catalogIndex{map[string]FieldEntry{}, map[string]map[string]bool{}, map[string]map[string]bool{}}
	for _, f := range cat.Fields {
		idx.known[f.id()] = f
		if idx.surfaces[f.Harness] == nil {
			idx.surfaces[f.Harness], idx.keys[f.Harness] = map[string]bool{}, map[string]bool{}
		}
		idx.surfaces[f.Harness][f.Surface] = true
		idx.keys[f.Harness][f.Key] = true
	}
	return idx
}

// additions says what is new in seen: surfaces, keys, and keys newly withheld; and of a first
// census, its size.
func (d *HarnessDrift) additions(idx catalogIndex, seen map[string]e2e.FieldRow) {
	newSurfaces := map[string]*SurfaceChange{}
	for _, id := range slices.Sorted(maps.Keys(seen)) {
		r := seen[id]
		c := FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
		if r.Version != d.Version {
			c.From = r.Version
		}
		known, isKnown := idx.known[FieldEntry{Harness: d.Harness, Surface: r.Surface, Key: r.Key}.id()]
		switch {
		case d.First:
		case !idx.surfaces[d.Harness][r.Surface]:
			s := newSurfaces[r.Surface]
			if s == nil {
				s = &SurfaceChange{Surface: r.Surface, From: c.From}
				newSurfaces[r.Surface] = s
			}
			if c.From == "" || (s.From != "" && versionLess(s.From, c.From)) {
				s.From = c.From
			}
			s.Keys++
			if !idx.keys[d.Harness][r.Key] {
				s.NewKeys++
				d.Added = append(d.Added, c)
			}
		case !isKnown:
			d.Added = append(d.Added, c)
		}
		if e2e.Withheld(r.Class, r.Kinds, r.Kept) {
			if isKnown && known.withheld() {
				d.StillWithheld = append(d.StillWithheld, c)
			} else {
				d.Withheld = append(d.Withheld, c)
			}
		}
	}
	for _, s := range slices.Sorted(maps.Keys(newSurfaces)) {
		d.NewSurfaces = append(d.NewSurfaces, *newSurfaces[s])
	}
	if d.First {
		surfaces := map[string]bool{}
		for _, r := range seen {
			surfaces[r.Surface] = true
		}
		d.Surfaces, d.Keys = len(surfaces), len(seen)
	}
}

// judgedBuilds are the builds judged for what is gone, oldest first: every build censused whole
// tonight for the first time, while no newer build is. A census a scenario failed before taking
// whole is no evidence of what a build does not send (a key that comes with an error path, or
// a scenario that did not run, would read as one gone), and nor is a re-run of a build censused
// whole. Each build is judged, so none becomes a baseline unjudged, and a key one build adds and
// the next drops is said gone.
func judgedBuilds(cat Catalog, n night) []string {
	var judged []string
	for v := range rowBuilds(n.rows) {
		if !n.partial(v) && !cat.censusedWhole(n.harness, v) && !cat.wholeAfter(n.harness, v) {
			judged = append(judged, v)
		}
	}
	slices.SortFunc(judged, func(a, b string) int {
		if versionLess(a, b) {
			return -1
		}
		return 1
	})
	return judged
}

// baseline is what the judged builds are judged against: the build (since), what it sent
// (base), and the builds judged against it in turn (chain).
type baseline struct {
	since string
	base  sending
	chain []string
	// earlier is set where since was judged by its earlier nights' whole censuses, not
	// tonight's; partial says its census tonight was partial, else it was not censused tonight.
	earlier string
	partial bool
}

// chooseBaseline is the newest older build censused whole, what its whole censuses saw
// (FieldEntry.Whole; a partial census is a failed run, where keys of an error path come). Where
// it is censused whole tonight too, it is what it sent tonight, one night against one, so what
// it sent on some earlier night alone is not gone; otherwise it is its earlier nights, and the
// digest says so. Where the catalog has none, but has the harness, it is the oldest build judged
// tonight, the rest judged against it.
func chooseBaseline(cat Catalog, n night, judged []string, first bool) (b baseline) {
	whole, ok := cat.lastWhole(n.harness, firstOr(judged))
	if !ok {
		if len(judged) > 1 && !first {
			return baseline{since: judged[0], base: n.sends(judged[0]), chain: judged[1:]}
		}
		return baseline{}
	}
	b = baseline{since: whole.Version, base: sending{map[string]FieldChange{}, whole.Surfaces}, chain: judged}
	for _, f := range cat.Fields {
		if f.Harness == n.harness && slices.Contains(f.Whole, whole.Version) {
			b.base.keys[fieldID(f.Surface, f.Key)] = FieldChange{Surface: f.Surface, Key: f.Key, Class: f.Class, Kinds: f.Kinds}
		}
	}
	switch again := n.sends(whole.Version); {
	case len(again.keys) == 0:
		b.earlier = whole.Version
	case n.partial(whole.Version):
		b.earlier, b.partial = whole.Version, true
	default:
		maps.DeleteFunc(b.base.keys, func(k string, _ FieldChange) bool { _, ok := again.keys[k]; return !ok })
		b.base.surfaces = slices.DeleteFunc(slices.Clone(b.base.surfaces), func(s string) bool { return !slices.Contains(again.surfaces, s) })
	}
	return b
}

// judgeChain says what each build of b's chain no longer sends that the build before it did,
// oldest first. A removal from an earlier build says the build it went in, and the later build
// that sends it again, if one does; one the chain drops more than once is said once, as its
// last drop.
func (d *HarnessDrift) judgeChain(n night, b baseline) {
	if len(b.chain) == 0 {
		return
	}
	last := b.chain[len(b.chain)-1]
	d.Chain = b.chain
	if last != d.Version {
		d.Judged = last
	}
	if b.since != d.Previous {
		d.Since = b.since
	}
	d.Earlier, d.EarlierPartial = b.earlier, b.partial
	removedAt, unseenAt := map[string]int{}, map[string]int{}
	base := b.base
	for i, v := range b.chain {
		in := ""
		if v != last {
			in = v
		}
		back := func(k, surface string) string {
			if in == "" {
				return ""
			}
			for _, w := range b.chain[i+1:] {
				later := n.sends(w)
				if _, ok := later.keys[k]; ok || (k == "" && slices.Contains(later.surfaces, surface)) {
					return w
				}
			}
			return ""
		}
		now := n.sends(v)
		for _, k := range slices.Sorted(maps.Keys(base.keys)) {
			c := base.keys[k]
			if _, kept := now.keys[k]; kept || !slices.Contains(now.surfaces, c.Surface) {
				continue
			}
			c.In, c.Back = in, back(k, "")
			if j, ok := removedAt[k]; ok {
				d.Removed[j] = c
				continue
			}
			removedAt[k] = len(d.Removed)
			d.Removed = append(d.Removed, c)
		}
		for _, s := range base.surfaces {
			if slices.Contains(now.surfaces, s) {
				continue
			}
			g := GoneSurface{Surface: s, In: in, Back: back("", s)}
			if j, ok := unseenAt[s]; ok {
				d.Unseen[j] = g
				continue
			}
			unseenAt[s] = len(d.Unseen)
			d.Unseen = append(d.Unseen, g)
		}
		base = now
	}
}
