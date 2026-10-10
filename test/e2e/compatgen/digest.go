package main

import (
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The digest is what a night's runs changed against the catalog and the history as last
// published: per harness, the newest build censused, the surfaces and keys that appeared,
// those a newer build no longer sends, and the keys the relay withholds as unclassified; and
// every capability whose result changed. A run that brought no census, or none of a harness
// the catalog saw lately, says so: a quiet night is one that took the census and found
// nothing. report/drift.json holds it, report/drift.md and report/slack.json say it, the
// second for a Slack incoming webhook.

// Drift is the digest of one night.
type Drift struct {
	GeneratedAt time.Time `json:"generated_at"`
	Link        string    `json:"link,omitempty"`
	// NoCensus is a night whose runs brought no field census at all: the job that takes it
	// failed before writing it, or could not classify it.
	NoCensus bool `json:"no_census,omitempty"`
	// Missing names the harnesses this night has no census of that it should have: those the
	// catalog took a census of within missingWithin, and those whose census scenarios ran.
	Missing   []string       `json:"missing,omitempty"`
	Harnesses []HarnessDrift `json:"harnesses"`
	// SourceErrors are the builds whose source could not be read, their findings unlinked.
	SourceErrors []string       `json:"source_errors,omitempty"`
	Compat       []CompatChange `json:"compat"`
}

// missingWithin is how recently a harness must have been censused for its absence to count:
// a harness the nightly job stopped running ages out of the digest in a week.
const missingWithin = 7 * 24 * time.Hour

// HarnessDrift is what changed for one harness.
type HarnessDrift struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	// Version is the newest build the night censused; Previous the newest the catalog had.
	Version  string `json:"version"`
	Previous string `json:"previous,omitempty"`
	// First is a harness the catalog had no census of; Surfaces and Keys its census's size.
	First bool `json:"first,omitempty"`
	// Partial is a census of Version a census scenario failed before taking whole: Removed
	// and Unseen are not judged.
	Partial bool `json:"partial,omitempty"`
	// Chain are the builds Removed and Unseen were judged on, oldest first, each against the
	// one before it, the first against Since, or Previous.
	Chain []string `json:"chain,omitempty"`
	// Judged is the last build Removed and Unseen were judged on, where it is not Version: the
	// newest censused whole tonight, Version's census partial.
	Judged string `json:"judged,omitempty"`
	// Since is the build Removed and Unseen were judged against, where it is not Previous: the
	// newest older build with a whole census, the ones after it partial.
	Since string `json:"since,omitempty"`
	// Earlier is the build judged against, where it was judged by its earlier nights' whole
	// censuses, its census tonight partial: what it sent only on some of them may read gone.
	Earlier string `json:"earlier,omitempty"`
	// Unreached is a build newer than Version that the census should have reached and did
	// not: Previous, the newest the catalog had (it failed to install, or its tests did not
	// run), or the newest tonight's census scenarios ran (they failed before its census).
	Unreached string `json:"unreached,omitempty"`
	Surfaces  int    `json:"surfaces,omitempty"`
	Keys      int    `json:"keys,omitempty"`
	// NewSurfaces are surfaces no build of the harness had (a span renamed is one, not each of
	// its keys); Added are keys new to their surface on a surface the catalog knew, or new to
	// the harness anywhere. Both are of every build tonight, whose rows all go into the
	// catalog tonight, Version's row first.
	NewSurfaces []SurfaceChange `json:"new_surfaces,omitempty"`
	Added       []FieldChange   `json:"added,omitempty"`
	// Removed and Unseen are what the build judged (Judged, or Version) no longer sends that
	// the newest older build censused whole (Since, or Previous) did: keys on a surface it
	// still sends, and surfaces. See fieldDrift for when a build is judged.
	Removed []FieldChange `json:"removed,omitempty"`
	Unseen  []GoneSurface `json:"unseen,omitempty"`
	// Compare links the source's changes from Previous to Version, where it is public.
	Compare string `json:"compare,omitempty"`
	// Withheld are the unclassified keys the relay drops, of the same builds as Added, that the
	// catalog did not have withheld; StillWithheld those it did, until each is classified.
	Withheld      []FieldChange `json:"withheld,omitempty"`
	StillWithheld []FieldChange `json:"still_withheld,omitempty"`
}

// changed reports whether h has anything new to say.
func (h HarnessDrift) changed() bool {
	return h.First || h.Partial || h.Unreached != "" || len(h.NewSurfaces)+len(h.Added)+len(h.Removed)+len(h.Unseen)+len(h.Withheld) > 0
}

// FieldChange is one key on one surface.
type FieldChange struct {
	Surface string   `json:"surface"`
	Key     string   `json:"key"`
	Class   string   `json:"class,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	// From is the build that sent it, of an addition, where it is not Version. In is the build
	// that no longer sends it, of a removal, where it is not the last build judged; Back the
	// later build judged that sends it again, if one does.
	From string `json:"from,omitempty"`
	In   string `json:"in,omitempty"`
	Back string `json:"back,omitempty"`
	// Source is where the harness's source names it, where it is public.
	Source *SourceSays `json:"source,omitempty"`
}

// SurfaceChange is a surface new to a harness, with how many keys it carried and how many of
// them the harness never sent before.
type SurfaceChange struct {
	Surface string `json:"surface"`
	Keys    int    `json:"keys"`
	NewKeys int    `json:"new_keys"`
	// From is the build that sent it, where Version did not: the newest of tonight's that did.
	From string `json:"from,omitempty"`
	// Source is where the harness's source names it, where it is public.
	Source *SourceSays `json:"source,omitempty"`
}

// GoneSurface is a surface the previous build sent and the new one did not.
type GoneSurface struct {
	Surface string `json:"surface"`
	// In is the build that no longer sends it, where it is not the last build judged; Back the
	// later build judged that sends it again, if one does.
	In     string      `json:"in,omitempty"`
	Back   string      `json:"back,omitempty"`
	Source *SourceSays `json:"source,omitempty"`
}

// Quiet reports a night that took the census of every harness it should have and found
// nothing worth a look.
func (d Drift) Quiet() bool {
	if d.NoCensus || len(d.Missing) > 0 || len(d.Compat) > 0 {
		return false
	}
	return !slices.ContainsFunc(d.Harnesses, HarnessDrift.changed)
}

// lastJudged is the last build what is gone was judged on: Judged, or Version.
func (h HarnessDrift) lastJudged() string {
	if h.Judged != "" {
		return h.Judged
	}
	return h.Version
}

// fieldDrift compares a night's census with the catalog before it is merged; ran is what
// censusRan says of the night.
func fieldDrift(cat Catalog, rows []e2e.FieldRow, ran censusRuns, now time.Time) (noCensus bool, missing []string, out []HarnessDrift) {
	byHarness := map[string][]e2e.FieldRow{}
	for _, r := range rows {
		byHarness[r.Harness] = append(byHarness[r.Harness], r)
	}
	for _, c := range cat.Censuses {
		if newest, _ := cat.newestCensus(c.Harness); newest.id() == c.id() && now.Sub(c.At) <= missingWithin && byHarness[c.Harness] == nil {
			missing = append(missing, harnessName(c.Harness))
		}
	}
	// A harness whose census scenarios ran tonight and took none is missing too, censused
	// before or never: else one that fails before its census every night reads as quiet.
	for harness := range ran.newest {
		if byHarness[harness] == nil && !slices.Contains(missing, harnessName(harness)) {
			missing = append(missing, harnessName(harness))
		}
	}
	slices.Sort(missing)
	known := map[string]FieldEntry{}
	surfaces := map[string]map[string]bool{} // harness → surfaces the catalog knows
	keys := map[string]map[string]bool{}     // harness → keys the catalog knows, on any surface
	for _, f := range cat.Fields {
		known[f.id()] = f
		if surfaces[f.Harness] == nil {
			surfaces[f.Harness], keys[f.Harness] = map[string]bool{}, map[string]bool{}
		}
		surfaces[f.Harness][f.Surface] = true
		keys[f.Harness][f.Key] = true
	}
	for _, harness := range slices.Sorted(maps.Keys(byHarness)) {
		hr := byHarness[harness]
		version := hr[0].Version
		for _, r := range hr {
			if versionLess(version, r.Version) {
				version = r.Version
			}
		}
		d := HarnessDrift{Harness: harness, Name: harnessName(harness), Version: version}
		d.Partial = ran.failed[harness+"\x00"+version]
		// What is new is new in any build tonight, the newest's row first: whatever the catalog
		// has not seen goes into it tonight, from a re-run's error path or a build's first whole
		// census as much as from a new build, and would never be new again.
		tonight := map[string]e2e.FieldRow{}
		sent := map[string]bool{}
		for _, r := range hr {
			if r.Version == version {
				tonight[r.Surface+"\x00"+r.Key] = r
				sent[r.Surface] = true
			}
		}
		others := slices.DeleteFunc(slices.Collect(maps.Keys(rowBuilds(hr))), func(v string) bool { return v == version })
		sortVersionsDesc(others)
		for _, v := range others {
			for _, r := range hr {
				if _, ok := tonight[r.Surface+"\x00"+r.Key]; !ok && r.Version == v {
					tonight[r.Surface+"\x00"+r.Key] = r
				}
			}
		}
		prev, censused := cat.newestCensus(harness)
		d.First = !censused
		if censused {
			d.Previous = prev.Version
			if versionLess(version, prev.Version) {
				d.Unreached = prev.Version
			}
		}
		if v := ran.newest[harness]; versionLess(version, v) && versionLess(d.Unreached, v) {
			d.Unreached = v
		}
		newSurfaces := map[string]*SurfaceChange{}
		for _, id := range slices.Sorted(maps.Keys(tonight)) {
			r := tonight[id]
			c := FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
			if r.Version != version {
				c.From = r.Version
			}
			newKey := !keys[harness][r.Key]
			switch {
			case d.First:
			case !surfaces[harness][r.Surface]:
				s := newSurfaces[r.Surface]
				if s == nil {
					s = &SurfaceChange{Surface: r.Surface, From: c.From}
					newSurfaces[r.Surface] = s
				}
				if c.From == "" || (s.From != "" && versionLess(s.From, c.From)) {
					s.From = c.From
				}
				s.Keys++
				if newKey {
					s.NewKeys++
					d.Added = append(d.Added, c)
				}
			default:
				if _, ok := known[FieldEntry{Harness: harness, Surface: r.Surface, Key: r.Key}.id()]; !ok {
					d.Added = append(d.Added, c)
				}
			}
			if e2e.Withheld(r.Class, r.Kinds, r.Kept) {
				if prev, ok := known[FieldEntry{Harness: harness, Surface: r.Surface, Key: r.Key}.id()]; ok && prev.withheld() {
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
			counted := map[string]bool{}
			for _, r := range tonight {
				counted[r.Surface] = true
			}
			d.Surfaces, d.Keys = len(counted), len(tonight)
		}
		// What is gone is judged on every build censused whole tonight for the first time, while
		// no newer build is, oldest first, each against the build before it: the first against the
		// newest older build censused whole, so a partial one between them hides nothing. A
		// census a scenario failed before taking whole is no evidence of what a build does not
		// send (a key that comes with an error path, or a scenario that did not run, would read
		// as one gone), and nor is a re-run of a build censused whole. Each build is judged, so
		// none becomes a baseline unjudged, and a key one build adds and the next drops is said
		// gone.
		var judged []string
		for v := range rowBuilds(hr) {
			if !ran.failed[harness+"\x00"+v] && !cat.censusedWhole(harness, v) && !cat.wholeAfter(harness, v) {
				judged = append(judged, v)
			}
		}
		slices.SortFunc(judged, func(a, b string) int {
			if versionLess(a, b) {
				return -1
			}
			return 1
		})
		// What each build sent, as tonight's rows say.
		type sending struct {
			keys     map[string]FieldChange
			surfaces []string
		}
		sends := func(v string) sending {
			keys, surfaces := map[string]FieldChange{}, map[string]bool{}
			for _, r := range hr {
				if r.Version == v {
					keys[r.Surface+"\x00"+r.Key] = FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
					surfaces[r.Surface] = true
				}
			}
			return sending{keys, slices.Sorted(maps.Keys(surfaces))}
		}
		// The baseline: the newest older build censused whole, what its whole census saw
		// (FieldEntry.Whole; a partial census is a failed run, where keys of an error path
		// come); where the catalog has none, but has the harness, the oldest build judged
		// tonight, the rest judged against it.
		var base sending
		since, chain := "", judged
		if whole, ok := cat.lastWhole(harness, firstOr(judged)); ok {
			since, base = whole.Version, sending{map[string]FieldChange{}, whole.Surfaces}
			for _, f := range cat.Fields {
				if f.Harness == harness && slices.Contains(f.Whole, whole.Version) {
					base.keys[f.Surface+"\x00"+f.Key] = FieldChange{Surface: f.Surface, Key: f.Key, Class: f.Class, Kinds: f.Kinds}
				}
			}
			// The catalog has every night's whole census of it; the build judged has tonight's.
			// Where it is censused whole tonight too, it is judged by what it sent tonight, one
			// night against one, so a key or surface it sent on some earlier night alone is not
			// gone. Where its census tonight is partial, it is judged by its earlier nights, and
			// the digest says so (Earlier).
			switch again := sends(since); {
			case len(again.keys) == 0:
			case ran.failed[harness+"\x00"+since]:
				d.Earlier = since
			default:
				maps.DeleteFunc(base.keys, func(k string, _ FieldChange) bool { _, ok := again.keys[k]; return !ok })
				base.surfaces = slices.DeleteFunc(slices.Clone(base.surfaces), func(s string) bool { return !slices.Contains(again.surfaces, s) })
			}
		} else if len(judged) > 1 && !d.First {
			since, base, chain = judged[0], sends(judged[0]), judged[1:]
		} else {
			chain = nil
		}
		if len(chain) > 0 {
			last := chain[len(chain)-1]
			d.Chain = chain
			if last != version {
				d.Judged = last
			}
			if since != d.Previous {
				d.Since = since
			}
			// A key or surface the chain drops more than once is said once, as its last drop.
			removedAt, unseenAt := map[string]int{}, map[string]int{}
			removed := func(c FieldChange) {
				k := c.Surface + "\x00" + c.Key
				if i, ok := removedAt[k]; ok {
					d.Removed[i] = c
					return
				}
				removedAt[k] = len(d.Removed)
				d.Removed = append(d.Removed, c)
			}
			unseen := func(g GoneSurface) {
				if i, ok := unseenAt[g.Surface]; ok {
					d.Unseen[i] = g
					return
				}
				unseenAt[g.Surface] = len(d.Unseen)
				d.Unseen = append(d.Unseen, g)
			}
			for i, v := range chain {
				in := ""
				if v != last {
					in = v
				}
				now := sends(v)
				sent := map[string]bool{}
				for _, s := range now.surfaces {
					sent[s] = true
				}
				// A key or surface an earlier build of the chain dropped and a later one sends
				// again says which: it is not gone for good.
				back := func(k, surface string) string {
					for _, w := range chain[i+1:] {
						later := sends(w)
						if _, ok := later.keys[k]; ok || (k == "" && slices.Contains(later.surfaces, surface)) {
							return w
						}
					}
					return ""
				}
				for _, k := range slices.Sorted(maps.Keys(base.keys)) {
					if c := base.keys[k]; sent[c.Surface] && now.keys[k].Key == "" {
						c.In = in
						if in != "" {
							c.Back = back(k, "")
						}
						removed(c)
					}
				}
				for _, s := range base.surfaces {
					if !sent[s] {
						g := GoneSurface{Surface: s, In: in}
						if in != "" {
							g.Back = back("", s)
						}
						unseen(g)
					}
				}
				base = now
			}
		}

		out = append(out, d)
	}
	return len(rows) == 0, missing, out
}

func harnessName(id string) string {
	for _, h := range e2e.Harnesses {
		if h.ID == id {
			return h.Name
		}
	}
	return id
}

func capabilityLabel(id string) string {
	for _, c := range e2e.Capabilities {
		if c.ID == id {
			return c.Label
		}
	}
	return id
}
