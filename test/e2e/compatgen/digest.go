package main

import (
	"maps"
	"slices"
	"sort"
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
	// In is the build that no longer sends it, of a removal, where it is not the last build
	// judged.
	In string `json:"in,omitempty"`
	// Source is where the harness's source names it, where it is public.
	Source *SourceSays `json:"source,omitempty"`
}

// SurfaceChange is a surface new to a harness, with how many keys it carried and how many of
// them the harness never sent before.
type SurfaceChange struct {
	Surface string `json:"surface"`
	Keys    int    `json:"keys"`
	NewKeys int    `json:"new_keys"`
	// Source is where the harness's source names it, where it is public.
	Source *SourceSays `json:"source,omitempty"`
}

// GoneSurface is a surface the previous build sent and the new one did not.
type GoneSurface struct {
	Surface string `json:"surface"`
	// In is the build that no longer sends it, where it is not the last build judged.
	In     string      `json:"in,omitempty"`
	Source *SourceSays `json:"source,omitempty"`
}

// CompatChange is a capability whose result for a build changed, or a build's first failure.
type CompatChange struct {
	Harness    string `json:"harness"`
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	Capability string `json:"capability"`
	From       string `json:"from,omitempty"`
	To         string `json:"to"`
}

// Quiet reports a night that took the census of every harness it should have and found
// nothing worth a look.
func (d Drift) Quiet() bool {
	if d.NoCensus || len(d.Missing) > 0 || len(d.Compat) > 0 {
		return false
	}
	return !slices.ContainsFunc(d.Harnesses, HarnessDrift.changed)
}

func firstOr(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// lastJudged is the last build what is gone was judged on: Judged, or Version.
func (h HarnessDrift) lastJudged() string {
	if h.Judged != "" {
		return h.Judged
	}
	return h.Version
}

// rowBuilds are the builds rows are of.
func rowBuilds(rows []e2e.FieldRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Version] = true
	}
	return out
}

// censusRuns is what tonight's census scenarios did (e2e.CensusRun, report/census.json): per
// harness, the newest build they ran, passed or failed; and the builds one of them failed
// for, whose census is partial.
type censusRuns struct {
	newest map[string]string
	failed map[string]bool // harness and version
}

func censusRan(runs []e2e.CensusRun) censusRuns {
	ran := censusRuns{newest: map[string]string{}, failed: map[string]bool{}}
	for _, r := range runs {
		if versionLess(ran.newest[r.Harness], r.Version) {
			ran.newest[r.Harness] = r.Version
		}
		if r.Failed {
			ran.failed[r.Harness+"\x00"+r.Version] = true
		}
	}
	return ran
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
			newKey := !keys[harness][r.Key]
			switch {
			case d.First:
			case !surfaces[harness][r.Surface]:
				s := newSurfaces[r.Surface]
				if s == nil {
					s = &SurfaceChange{Surface: r.Surface}
					newSurfaces[r.Surface] = s
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
			d.Surfaces, d.Keys = len(sent), len(tonight)
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
		if whole, ok := cat.lastWhole(harness, firstOr(judged)); ok && len(judged) > 0 {
			last := judged[len(judged)-1]
			d.Chain = judged
			if last != version {
				d.Judged = last
			}
			if whole.Version != d.Previous {
				d.Since = whole.Version
			}
			// Only what a whole census saw is evidence (FieldEntry.Whole): a partial census is a
			// failed run, where keys of an error path come.
			base := map[string]FieldChange{}
			for _, f := range cat.Fields {
				if f.Harness == harness && slices.Contains(f.Whole, whole.Version) {
					base[f.Surface+"\x00"+f.Key] = FieldChange{Surface: f.Surface, Key: f.Key, Class: f.Class, Kinds: f.Kinds}
				}
			}
			baseSurfaces := whole.Surfaces
			for _, v := range judged {
				in := ""
				if v != last {
					in = v
				}
				had, sentThen := map[string]FieldChange{}, map[string]bool{}
				for _, r := range hr {
					if r.Version == v {
						had[r.Surface+"\x00"+r.Key] = FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
						sentThen[r.Surface] = true
					}
				}
				for _, k := range slices.Sorted(maps.Keys(base)) {
					if c := base[k]; sentThen[c.Surface] && had[k].Key == "" {
						c.In = in
						d.Removed = append(d.Removed, c)
					}
				}
				for _, s := range baseSurfaces {
					if !sentThen[s] {
						d.Unseen = append(d.Unseen, GoneSurface{Surface: s, In: in})
					}
				}
				base, baseSurfaces = had, slices.Sorted(maps.Keys(sentThen))
			}
		}
		out = append(out, d)
	}
	return len(rows) == 0, missing, out
}

// compatDrift compares a night's results with the history before they are merged.
func compatDrift(hist map[string]Entry, rows []e2e.CompatRow) []CompatChange {
	var out []CompatChange
	for _, r := range rows {
		e := Entry{Harness: r.Harness, Version: r.Version, Platform: r.Platform, Capability: r.Capability}
		prev, ok := hist[e.key()]
		switch {
		case r.Result == "not run":
		case !ok && r.Result == "fail":
			out = append(out, CompatChange{r.Harness, r.Version, r.Platform, r.Capability, "", r.Result})
		case ok && prev.Result != "not run" && prev.Result != r.Result:
			out = append(out, CompatChange{r.Harness, r.Version, r.Platform, r.Capability, prev.Result, r.Result})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Harness+a.Version+a.Platform+a.Capability < b.Harness+b.Version+b.Platform+b.Capability
	})
	return out
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
