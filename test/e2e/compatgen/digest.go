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
	// censuses, not tonight's: what it sent only on some of them may read gone. EarlierPartial
	// says its census tonight was partial; otherwise it was not censused tonight.
	Earlier        string `json:"earlier,omitempty"`
	EarlierPartial bool   `json:"earlier_partial,omitempty"`
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
	idx := indexCatalog(cat)
	for _, harness := range slices.Sorted(maps.Keys(byHarness)) {
		n := night{harness: harness, rows: byHarness[harness], ran: ran}
		for v := range rowBuilds(n.rows) {
			if versionLess(n.newest, v) {
				n.newest = v
			}
		}
		d := HarnessDrift{Harness: harness, Name: harnessName(harness), Version: n.newest, Partial: n.partial(n.newest)}
		prev, censused := cat.newestCensus(harness)
		d.First = !censused
		if censused {
			d.Previous = prev.Version
			if versionLess(d.Version, prev.Version) {
				d.Unreached = prev.Version
			}
		}
		if v := ran.newest[harness]; versionLess(d.Version, v) && versionLess(d.Unreached, v) {
			d.Unreached = v
		}
		d.additions(idx, n.seen())
		d.judgeChain(n, chooseBaseline(cat, n, judgedBuilds(cat, n), d.First))
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
