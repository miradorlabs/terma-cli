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
	// Unreached is a build newer than Version that the census should have reached and did
	// not: Previous, the newest the catalog had (it failed to install, or its tests did not
	// run), or the newest tonight's census scenarios ran (they failed before its census).
	Unreached string `json:"unreached,omitempty"`
	Surfaces  int    `json:"surfaces,omitempty"`
	Keys      int    `json:"keys,omitempty"`
	// NewSurfaces are surfaces no build of the harness had (a span renamed is one, not each of
	// its keys); Added are keys new to their surface on a surface the catalog knew, or new to
	// the harness anywhere.
	NewSurfaces []SurfaceChange `json:"new_surfaces,omitempty"`
	Added       []FieldChange   `json:"added,omitempty"`
	// Removed and Unseen are judged only when Version is newer than Previous: keys Previous
	// had on a surface Version still sends, without them, and surfaces Version does not send.
	// A re-run of the same build is no evidence: a key that comes with an error path, or
	// a scenario that did not run, would read as one gone.
	Removed []FieldChange `json:"removed,omitempty"`
	Unseen  []GoneSurface `json:"unseen,omitempty"`
	// Compare links the source's changes from Previous to Version, where it is public.
	Compare string `json:"compare,omitempty"`
	// Withheld are Version's unclassified keys with text values, which the relay drops, that
	// the catalog did not have withheld; StillWithheld those it did, until each is classified.
	Withheld      []FieldChange `json:"withheld,omitempty"`
	StillWithheld []FieldChange `json:"still_withheld,omitempty"`
}

// changed reports whether h has anything new to say.
func (h HarnessDrift) changed() bool {
	return h.First || h.Unreached != "" || len(h.NewSurfaces)+len(h.Added)+len(h.Removed)+len(h.Unseen)+len(h.Withheld) > 0
}

// FieldChange is one key on one surface.
type FieldChange struct {
	Surface string   `json:"surface"`
	Key     string   `json:"key"`
	Class   string   `json:"class,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
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
	Surface string      `json:"surface"`
	Source  *SourceSays `json:"source,omitempty"`
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

// censusRan names, per harness, the newest build tonight's census scenarios ran: what proves
// e2e.CensusCapability, passed or failed.
func censusRan(rows []e2e.CompatRow) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		if r.Capability == e2e.CensusCapability && r.Result != "not run" && versionLess(out[r.Harness], r.Version) {
			out[r.Harness] = r.Version
		}
	}
	return out
}

// fieldDrift compares a night's census with the catalog before it is merged; ran is what
// censusRan says of the night.
func fieldDrift(cat Catalog, rows []e2e.FieldRow, ran map[string]string, now time.Time) (noCensus bool, missing []string, out []HarnessDrift) {
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
	for harness := range ran {
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
		tonight := map[string]e2e.FieldRow{}
		sent := map[string]bool{}
		for _, r := range hr {
			if r.Version == version {
				tonight[r.Surface+"\x00"+r.Key] = r
				sent[r.Surface] = true
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
		if v := ran[harness]; versionLess(version, v) && versionLess(d.Unreached, v) {
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
		if censused && versionLess(prev.Version, version) {
			for _, f := range cat.Fields {
				if f.Harness != harness || !slices.Contains(f.Versions, prev.Version) || !sent[f.Surface] {
					continue
				}
				if _, ok := tonight[f.Surface+"\x00"+f.Key]; !ok {
					d.Removed = append(d.Removed, FieldChange{Surface: f.Surface, Key: f.Key, Class: f.Class, Kinds: f.Kinds})
				}
			}
			for _, s := range prev.Surfaces {
				if !sent[s] {
					d.Unseen = append(d.Unseen, GoneSurface{Surface: s})
				}
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
