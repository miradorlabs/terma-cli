package main

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
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
	// Missing names the harnesses the catalog took a census of within missingWithin that this
	// night has none of.
	Missing   []string       `json:"missing,omitempty"`
	Harnesses []HarnessDrift `json:"harnesses"`
	Compat    []CompatChange `json:"compat"`
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
	First    bool `json:"first,omitempty"`
	Surfaces int  `json:"surfaces,omitempty"`
	Keys     int  `json:"keys,omitempty"`
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
	Unseen  []string      `json:"unseen,omitempty"`
	// Withheld are Version's unclassified keys with text values, which the relay drops, that
	// the catalog did not have withheld; StillWithheld those it did, until each is classified.
	Withheld      []FieldChange `json:"withheld,omitempty"`
	StillWithheld []FieldChange `json:"still_withheld,omitempty"`
}

// changed reports whether h has anything new to say.
func (h HarnessDrift) changed() bool {
	return h.First || len(h.NewSurfaces)+len(h.Added)+len(h.Removed)+len(h.Unseen)+len(h.Withheld) > 0
}

// FieldChange is one key on one surface.
type FieldChange struct {
	Surface string   `json:"surface"`
	Key     string   `json:"key"`
	Class   string   `json:"class,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
}

// SurfaceChange is a surface new to a harness, with how many keys it carried and how many of
// them the harness never sent before.
type SurfaceChange struct {
	Surface string `json:"surface"`
	Keys    int    `json:"keys"`
	NewKeys int    `json:"new_keys"`
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

// fieldDrift compares a night's census with the catalog before it is merged.
func fieldDrift(cat Catalog, rows []e2e.FieldRow, now time.Time) (noCensus bool, missing []string, out []HarnessDrift) {
	byHarness := map[string][]e2e.FieldRow{}
	for _, r := range rows {
		byHarness[r.Harness] = append(byHarness[r.Harness], r)
	}
	for _, c := range cat.Censuses {
		if newest, _ := cat.newestCensus(c.Harness); newest.id() == c.id() && now.Sub(c.At) <= missingWithin && byHarness[c.Harness] == nil {
			missing = append(missing, harnessName(c.Harness))
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
			if withheldUnclassified(r.Class, r.Kinds) {
				if prev, ok := known[FieldEntry{Harness: harness, Surface: r.Surface, Key: r.Key}.id()]; ok && withheldUnclassified(prev.Class, prev.Kinds) {
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
					d.Unseen = append(d.Unseen, s)
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

// --- saying it -----------------------------------------------------------------------

// maxListed bounds each list a message spells out; the rest is counted.
const maxListed = 12

func (h HarnessDrift) headline() string {
	if h.First {
		return fmt.Sprintf("%s %s: first census, %s, %s", h.Name, h.Version, plural(h.Surfaces, "surface"), plural(h.Keys, "key"))
	}
	build := h.Version
	if h.Previous != h.Version {
		build += " (was " + h.Previous + ")"
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{
		{len(h.NewSurfaces), "new surface"}, {len(h.Added), "new field"}, {len(h.Removed), "removed field"},
		{len(h.Unseen), "surface no longer sent"}, {len(h.Withheld), "newly withheld field"},
	} {
		if p.n > 0 {
			parts = append(parts, plural(p.n, p.what))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "no changes")
	}
	return h.Name + " " + build + ": " + strings.Join(parts, ", ")
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// listed spells out at most maxListed lines, and counts the rest.
func listed(lines []string) []string {
	if len(lines) <= maxListed {
		return lines
	}
	return append(slices.Clip(lines[:maxListed]), fmt.Sprintf("… and %d more", len(lines)-maxListed))
}

func changeLines(cs []FieldChange, withClass bool) []string {
	var out []string
	for _, c := range cs {
		line := "`" + c.Key + "` on `" + c.Surface + "`"
		if withClass && c.Class != "" {
			line += " (" + classLabel[c.Class] + ")"
		}
		out = append(out, line)
	}
	return listed(out)
}

func surfaceLines(ss []SurfaceChange) []string {
	var out []string
	for _, s := range ss {
		out = append(out, fmt.Sprintf("`%s` (%s, %d new)", s.Surface, plural(s.Keys, "key"), s.NewKeys))
	}
	return listed(out)
}

func codeLines(ss []string) []string {
	var out []string
	for _, s := range ss {
		out = append(out, "`"+s+"`")
	}
	return listed(out)
}

// sections are what a harness's change says, titled.
func (h HarnessDrift) sections() []struct {
	title string
	lines []string
} {
	return []struct {
		title string
		lines []string
	}{
		{"New surfaces", surfaceLines(h.NewSurfaces)},
		{"New fields", changeLines(h.Added, true)},
		{"Removed fields", changeLines(h.Removed, true)},
		{"Surfaces no longer sent", codeLines(h.Unseen)},
		{"Newly withheld by the relay, unclassified", changeLines(h.Withheld, false)},
	}
}

func (c CompatChange) line() string {
	from := c.From
	if from == "" {
		from = "new build"
	}
	return fmt.Sprintf("%s %s · %s · %s: %s → %s", harnessName(c.Harness), c.Version, platformShort(c.Platform), capabilityLabel(c.Capability), from, c.To)
}

// alarms are what a night failed to do: take the census, or take it of every harness.
func (d Drift) alarms() []string {
	var out []string
	if d.NoCensus {
		out = append(out, "No field census reached the digest: the job that takes it failed before writing it, or could not classify it. See the run.")
	}
	if len(d.Missing) > 0 {
		out = append(out, "No census this night of "+strings.Join(d.Missing, ", ")+", censused within the week: its tests did not run, or its exporter sent nothing.")
	}
	return out
}

// unchanged names the harness builds with nothing new, as one line.
func (d Drift) unchanged() string {
	var builds []string
	for _, h := range d.Harnesses {
		if h.changed() {
			continue
		}
		b := h.Name + " " + h.Version
		if h.Previous != h.Version {
			b += " (new build, was " + h.Previous + ")"
		}
		builds = append(builds, b)
	}
	return strings.Join(builds, ", ")
}

// stillWithheld is the reminder of what the relay has been withholding, unclassified, before.
func (d Drift) stillWithheld() string {
	var parts []string
	for _, h := range d.Harnesses {
		keys := map[string]bool{}
		for _, c := range h.StillWithheld {
			keys["`"+c.Key+"`"] = true
		}
		if len(keys) > 0 {
			parts = append(parts, h.Name+": "+strings.Join(slices.Sorted(maps.Keys(keys)), ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Still withheld as unclassified, until each is classified: " + strings.Join(parts, "; ") + "."
}

// markdown is the digest for the run's summary and report/drift.md.
func (d Drift) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Harness drift — %s\n\n", d.GeneratedAt.Format("2006-01-02"))
	for _, a := range d.alarms() {
		b.WriteString("**" + a + "**\n\n")
	}
	if d.Quiet() {
		b.WriteString("No changes: every field and capability as the catalog and history have them.\n\n")
	}
	if u := d.unchanged(); u != "" {
		fmt.Fprintf(&b, "Unchanged: %s.\n\n", u)
	}
	if s := d.stillWithheld(); s != "" {
		b.WriteString(s + "\n\n")
	}
	for _, h := range d.Harnesses {
		if !h.changed() {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", h.headline())
		for _, sec := range h.sections() {
			if len(sec.lines) == 0 {
				continue
			}
			fmt.Fprintf(&b, "**%s**\n\n", sec.title)
			for _, l := range sec.lines {
				b.WriteString("- " + l + "\n")
			}
			b.WriteString("\n")
		}
	}
	if len(d.Compat) > 0 {
		b.WriteString("## Compatibility changes\n\n")
		for _, c := range d.Compat {
			b.WriteString("- " + c.line() + "\n")
		}
		b.WriteString("\n")
	}
	if d.Link != "" {
		fmt.Fprintf(&b, "[The run](%s)\n", d.Link)
	}
	return b.String()
}

// sectionLimit keeps a Slack section's text under its 3000-character limit.
const sectionLimit = 2900

// clip cuts s to at most limit bytes at the end of a line, so neither a character nor a code
// span is split, and says so.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut <= 0 {
		cut = limit
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
	}
	return s[:cut] + "\n…"
}

// slack is the digest as a Slack incoming webhook's payload: a header, the alarms, a section
// per harness that changed and one for compatibility, and a link to the run.
func (d Drift) slack() map[string]any {
	type block = map[string]any
	text := func(s string) block { return block{"type": "mrkdwn", "text": s} }
	section := func(s string) block { return block{"type": "section", "text": text(clip(s, sectionLimit))} }
	summary := "Harness drift: no changes"
	if !d.Quiet() {
		summary = "Harness drift: changes to look at"
	}
	blocks := []block{{"type": "header", "text": block{"type": "plain_text", "text": "Harness drift — " + d.GeneratedAt.Format("2006-01-02")}}}
	for _, a := range d.alarms() {
		blocks = append(blocks, section(":warning: *"+a+"*"))
	}
	if d.Quiet() {
		blocks = append(blocks, section("No changes. Censused: "+d.unchanged()+"."))
	}
	for _, h := range d.Harnesses {
		if !h.changed() {
			continue
		}
		var b strings.Builder
		b.WriteString("*" + h.headline() + "*")
		for _, sec := range h.sections() {
			if len(sec.lines) > 0 {
				b.WriteString("\n_" + sec.title + "_\n• " + strings.Join(sec.lines, "\n• "))
			}
		}
		blocks = append(blocks, section(b.String()))
	}
	if u := d.unchanged(); u != "" && !d.Quiet() {
		blocks = append(blocks, section("_Unchanged:_ "+u))
	}
	if s := d.stillWithheld(); s != "" {
		blocks = append(blocks, section(s))
	}
	if len(d.Compat) > 0 {
		var lines []string
		for _, c := range d.Compat {
			lines = append(lines, c.line())
		}
		blocks = append(blocks, section("*Compatibility changes*\n• "+strings.Join(listed(lines), "\n• ")))
	}
	if d.Link != "" {
		blocks = append(blocks, block{"type": "context", "elements": []block{text("<" + d.Link + "|The run> · the catalog and the matrix are on the compat-matrix branch")}})
	}
	return map[string]any{"text": summary, "blocks": blocks}
}
