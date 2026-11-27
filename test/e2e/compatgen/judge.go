package main

import (
	"maps"
	"slices"
	"strings"

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
			keys[fieldID(r.Surface, r.Key)] = FieldChange{Surface: r.Surface, Key: r.Key, Event: r.Event, Class: r.Class, Kinds: r.Kinds}
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
	slices.SortFunc(judged, compareVersions)
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
// it is censused whole tonight too, it is all it sent tonight, one night against one: what it
// sent on some earlier night alone is not gone, and what tonight's census reached first is
// there for the next build to drop. Otherwise it is its earlier nights, and the digest says so.
// Where the catalog has none, but has the harness, it is the oldest build judged tonight, the
// rest judged against it.
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
			b.base.keys[fieldID(f.Surface, f.Key)] = FieldChange{Surface: f.Surface, Key: f.Key, Event: f.Event, Class: f.Class, Kinds: f.Kinds}
		}
	}
	switch again := n.sends(whole.Version); {
	case len(again.keys) == 0:
		b.earlier = whole.Version
	case n.partial(whole.Version):
		b.earlier, b.partial = whole.Version, true
	default:
		b.base = again
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
	var judgedAgainst sending
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
		judgedAgainst, base = base, now
	}
	d.pairMoves(n, last, judgedAgainst, base)
}

// pairMoves turns a gone and a new surface of the last build judged into one Moved, taking them
// out of Unseen and NewSurfaces, where they are the same span event on another span: before, the
// build last was judged against, sent the old one as a span event and last sends the new one as
// the same event (e2e.FieldRow.Event), with the same keys, and
//   - the old span went in last itself, not in an earlier build;
//   - no build older than last sent the new span tonight, its census whole or partial: the new
//     span is that build's new surface (fieldsOf);
//   - no other surface gone in last or new carries the event (tracing names an event after its
//     file, so names repeat).
//
// A move has no source note. A surface whose event no census has named yet (a catalog entry filed
// before rows carried it) is no span event, so it is said gone or new, as before.
//
// Accepted risk: another event from the same file that went from one span while a new span got
// one with the same keys reads as a move, hiding a surface no longer sent; the upgrade path is to
// match on the event.name value, which the census does not keep.
func (d *HarnessDrift) pairMoves(n night, last string, before, now sending) {
	gone, came := map[string][]int{}, map[string][]int{}
	for i, g := range d.Unseen {
		if event := eventOf(before, g.Surface); event != "" && g.In == "" {
			gone[event] = append(gone[event], i)
		}
	}
	for i, s := range d.NewSurfaces {
		if event := eventOf(now, s.Surface); event != "" {
			came[event] = append(came[event], i)
		}
	}
	moved, arrived := map[int]bool{}, map[int]bool{}
	for _, event := range slices.Sorted(maps.Keys(gone)) {
		if len(gone[event]) != 1 || len(came[event]) != 1 {
			continue
		}
		g, s := d.Unseen[gone[event][0]], d.NewSurfaces[came[event][0]]
		keys := keysOn(now, s.Surface)
		if n.sentBefore(last, s.Surface) || !slices.Equal(keysOn(before, g.Surface), keys) {
			continue
		}
		d.Moved = append(d.Moved, MovedEvent{Event: event, From: spanOf(g.Surface, event), To: spanOf(s.Surface, event), Keys: len(keys)})
		moved[gone[event][0]], arrived[came[event][0]] = true, true
	}
	d.Unseen = dropMarked(d.Unseen, moved)
	d.NewSurfaces = dropMarked(d.NewSurfaces, arrived)
}

// sentBefore reports whether a build older than version sent surface tonight.
func (n night) sentBefore(version, surface string) bool {
	return slices.ContainsFunc(n.rows, func(r e2e.FieldRow) bool { return r.Surface == surface && versionLess(r.Version, version) })
}

// keysOn are the keys s sent on surface, sorted.
func keysOn(s sending, surface string) []string {
	var keys []string
	for _, c := range s.keys {
		if c.Surface == surface {
			keys = append(keys, c.Key)
		}
	}
	slices.Sort(keys)
	return keys
}

// eventOf is the span event surface is, as s sent it; none for a surface that is no span event.
func eventOf(s sending, surface string) string {
	for _, c := range s.keys {
		if c.Surface == surface && c.Event != "" {
			return c.Event
		}
	}
	return ""
}

// spanOf is the span a span event surface is on: traces/<span>/events/<event>.
func spanOf(surface, event string) string {
	return strings.TrimSuffix(strings.TrimPrefix(surface, "traces/"), "/events/"+event)
}

// dropMarked is s without the elements whose index is marked.
func dropMarked[T any](s []T, marked map[int]bool) []T {
	out := s[:0]
	for i, v := range s {
		if !marked[i] {
			out = append(out, v)
		}
	}
	return out
}
