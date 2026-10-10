package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The field catalog (docs/compat/fields.json) is what terma knows each harness emits: every
// attribute key on every surface, the builds it was seen in, the kinds of value it carried,
// and what terma's relay does with it. It lives beside the history on the compat-matrix
// branch, which the nightly job extends; each run's census (report/fields.json) is merged into
// it, and docs/FIELDS.md renders it. It is bounded: a key keeps the first build it was seen
// in and the newest keepVersions, a harness its newest keepCensuses censuses.

// Catalog is the field catalog.
type Catalog struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Censuses    []Census     `json:"censuses"`
	Fields      []FieldEntry `json:"fields"`
}

// Census is one harness build a run took the census of: the surfaces it saw, so a key is
// judged gone only where its surface was seen.
type Census struct {
	Harness  string    `json:"harness"`
	Version  string    `json:"version"`
	Surfaces []string  `json:"surfaces"`
	At       time.Time `json:"at"`
	// Partial is a census a census scenario failed before taking whole: what it did not see
	// is no evidence the build does not send it.
	Partial bool `json:"partial,omitempty"`
}

// FieldEntry is one key a harness emitted on one surface.
type FieldEntry struct {
	Harness string `json:"harness"`
	Surface string `json:"surface"`
	Key     string `json:"key"`
	// Class is what the relay of the latest run that saw it does with it.
	Class string   `json:"class"`
	Kinds []string `json:"kinds"`
	// Kept are the kinds of value the relay keeps of it, unclassified, as the latest run that
	// saw it says.
	Kept []string `json:"kept,omitempty"`
	// FirstSeen is the oldest build the catalog saw it in; Versions the newest it was seen in,
	// newest first, at most keepVersions.
	FirstSeen string   `json:"first_seen"`
	Versions  []string `json:"versions"`
	// Whole are the newest builds a whole census saw it in, newest first, at most
	// keepVersions: the evidence a build sent it. A build's partial census, a failed run, sees
	// keys of an error path, so what only one saw is in Versions alone, though the build is
	// censused whole another night.
	Whole []string `json:"whole,omitempty"`
}

const (
	keepVersions = 5
	keepCensuses = 5
)

// withheld reports a key the relay drops, unclassified, under a policy that withholds content.
func (f FieldEntry) withheld() bool { return e2e.Withheld(f.Class, f.Kinds, f.Kept) }

func (f FieldEntry) id() string { return f.Harness + "\x00" + f.Surface + "\x00" + f.Key }

func (c Census) id() string { return c.Harness + "\x00" + c.Version }

// mergeFields files a run's census in the catalog, partial naming the builds whose census is
// (censusRuns.failed). Runs are merged in the order they ran, so a key's class is the latest
// run's.
func mergeFields(cat *Catalog, rows []e2e.FieldRow, partial map[string]bool) {
	fields := map[string]*FieldEntry{}
	for i := range cat.Fields {
		fields[cat.Fields[i].id()] = &cat.Fields[i]
	}
	var added []*FieldEntry
	type build struct{ harness, version string }
	seen := map[build]map[string]bool{}
	at := map[build]time.Time{}
	for _, r := range rows {
		b := build{r.Harness, r.Version}
		if seen[b] == nil {
			seen[b] = map[string]bool{}
		}
		seen[b][r.Surface] = true
		if r.At.After(at[b]) {
			at[b] = r.At
		}
		e := FieldEntry{Harness: r.Harness, Surface: r.Surface, Key: r.Key}
		f, ok := fields[e.id()]
		if !ok {
			f = &FieldEntry{Harness: r.Harness, Surface: r.Surface, Key: r.Key, FirstSeen: r.Version, Kinds: slices.Clone(r.Kinds), Class: r.Class, Kept: slices.Clone(r.Kept)}
			added = append(added, f)
			fields[e.id()] = f
		}
		if f.FirstSeen == "" || versionLess(r.Version, f.FirstSeen) {
			f.FirstSeen = r.Version
		}
		newest := ""
		if len(f.Versions) > 0 {
			newest = f.Versions[0]
		}
		if !slices.Contains(f.Versions, r.Version) {
			f.Versions = append(f.Versions, r.Version)
			sortVersionsDesc(f.Versions)
			f.Versions = f.Versions[:min(len(f.Versions), keepVersions)]
		}
		// A whole run's kinds replace those a failed run of the build left (keys of an error path
		// may carry text), and a failed run adds none to a build censused whole: as its census's
		// surfaces are.
		failedRun := partial[r.Harness+"\x00"+r.Version]
		wasWhole := slices.Contains(f.Whole, r.Version)
		if !failedRun && !wasWhole {
			f.Whole = append(f.Whole, r.Version)
			sortVersionsDesc(f.Whole)
			f.Whole = f.Whole[:min(len(f.Whole), keepVersions)]
		}
		// The kinds are those of the newest build a whole census saw it in: a key an old build
		// sent as text and a new one as a number is no longer withheld. That build's first whole
		// run replaces what was there, a failed run's included, and a later one adds to it; a
		// failed run sets them only while no whole census has seen the key, and an older
		// build's whole run never. Its class, and what the relay keeps of it, are the latest
		// run's, failed or whole: they are the terma under test's answer, not the harness's, so
		// a key classified since is no longer withheld either.
		newestWhole := !failedRun && f.Whole[0] == r.Version                       // the newest build censused whole
		onlyFailed := failedRun && len(f.Whole) == 0 && f.Versions[0] == r.Version // no whole census yet
		switch {
		case newestWhole && !wasWhole, onlyFailed && newest != r.Version:
			f.Kinds = slices.Clone(r.Kinds)
		case newestWhole, onlyFailed:
			for _, k := range r.Kinds {
				if !slices.Contains(f.Kinds, k) {
					f.Kinds = append(f.Kinds, k)
				}
			}
			slices.Sort(f.Kinds)
		}
		if r.Class != "" {
			f.Class, f.Kept = r.Class, slices.Clone(r.Kept)
		}

	}
	for _, f := range added {
		cat.Fields = append(cat.Fields, *f)
	}
	// New censuses are appended once the existing ones are updated: an append that grows the
	// slice would leave a pointer into it writing to the old array.
	censuses := map[string]*Census{}
	for i := range cat.Censuses {
		censuses[cat.Censuses[i].id()] = &cat.Censuses[i]
	}
	var newCensuses []Census
	for b, surfaces := range seen {
		c := Census{Harness: b.harness, Version: b.version, Surfaces: slices.Sorted(maps.Keys(surfaces)), At: at[b], Partial: partial[b.harness+"\x00"+b.version]}
		if prev, ok := censuses[c.id()]; ok {
			// A census's surfaces are its whole runs' once it has one: a partial run's, a
			// failed run's, are no evidence of what the build sends.
			switch {
			case prev.Partial && !c.Partial:
				prev.Surfaces, prev.Partial = c.Surfaces, false
			case !prev.Partial && c.Partial:
			default:
				for _, s := range prev.Surfaces {
					surfaces[s] = true
				}
				prev.Surfaces = slices.Sorted(maps.Keys(surfaces))
			}
			if c.At.After(prev.At) {
				prev.At = c.At
			}
			continue
		}
		newCensuses = append(newCensuses, c)
	}
	cat.Censuses = append(cat.Censuses, newCensuses...)
	sort.Slice(cat.Fields, func(i, j int) bool { return cat.Fields[i].id() < cat.Fields[j].id() })
	sort.Slice(cat.Censuses, func(i, j int) bool {
		a, b := cat.Censuses[i], cat.Censuses[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return versionLess(b.Version, a.Version)
	})
	// A harness keeps its newest censuses, and as many whole ones, older if need be: partial
	// censuses must not push out the whole ones that say what a build sends. A key stays only
	// while one of them saw it.
	kept := cat.Censuses[:0]
	count, whole := map[string]int{}, map[string]int{}
	censused := map[string]bool{} // harness and version
	for _, c := range cat.Censuses {
		if count[c.Harness] < keepCensuses || (!c.Partial && whole[c.Harness] < keepCensuses) {
			kept = append(kept, c)
			censused[c.id()] = true
			count[c.Harness]++
			if !c.Partial {
				whole[c.Harness]++
			}
		}
	}
	cat.Censuses = kept
	cat.Fields = slices.DeleteFunc(cat.Fields, func(f FieldEntry) bool {
		kept := func(v string) bool { return censused[f.Harness+"\x00"+v] }
		return !slices.ContainsFunc(f.Versions, kept) && !slices.ContainsFunc(f.Whole, kept)
	})
}

func sortVersionsDesc(vs []string) {
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[j], vs[i]) })
}

// lastWhole is the newest build of harness older than version the catalog took a whole census
// of: what a build is judged against for what it no longer sends.
func (cat *Catalog) lastWhole(harness, version string) (Census, bool) {
	var best Census
	found := false
	for _, c := range cat.Censuses {
		if c.Harness == harness && !c.Partial && versionLess(c.Version, version) && (!found || versionLess(best.Version, c.Version)) {
			best, found = c, true
		}
	}
	return best, found
}

// wholeAfter reports whether the catalog took a whole census of harness at a build newer than
// version.
func (cat *Catalog) wholeAfter(harness, version string) bool {
	return slices.ContainsFunc(cat.Censuses, func(c Census) bool {
		return c.Harness == harness && !c.Partial && versionLess(version, c.Version)
	})
}

// censusedWhole reports whether the catalog took a whole census of harness at version.
func (cat *Catalog) censusedWhole(harness, version string) bool {
	return slices.ContainsFunc(cat.Censuses, func(c Census) bool {
		return c.Harness == harness && c.Version == version && !c.Partial
	})
}

// shown is the census FIELDS.md shows harness as: its newest whole one, since what a partial
// census did not see is no evidence of absence, or its newest if none is whole; and its
// newest.
func (cat *Catalog) shown(harness string) (shown, newest Census, ok bool) {
	newest, ok = cat.newestCensus(harness)
	if !ok {
		return Census{}, Census{}, false
	}
	shown = newest
	if newest.Partial {
		if whole, found := cat.lastWhole(harness, newest.Version); found {
			shown = whole
		}
	}
	return shown, newest, true
}

// sentBy reports a key the build at version, or one after it, sent.
func (f FieldEntry) sentBy(version string) bool {
	return slices.ContainsFunc(f.Whole, func(v string) bool { return !versionLess(v, version) })
}

// seenBy reports a key a build at version, or one after it, sent in any run, failed ones too.
func (f FieldEntry) seenBy(version string) bool {
	return slices.ContainsFunc(f.Versions, func(v string) bool { return !versionLess(v, version) })
}

// newestCensus is the newest build of harness the catalog took the census of.
func (cat *Catalog) newestCensus(harness string) (Census, bool) {
	var best Census
	found := false
	for _, c := range cat.Censuses {
		if c.Harness == harness && (!found || versionLess(best.Version, c.Version)) {
			best, found = c, true
		}
	}
	return best, found
}

// writeCatalog writes cat one entry a line, so a night's change to the catalog is a diff of
// the entries it changed.
func writeCatalog(path string, cat Catalog) error {
	var b bytes.Buffer
	at, err := json.Marshal(cat.GeneratedAt)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "{\n  \"generated_at\": %s,\n", at)
	list := func(name string, n int, entry func(int) any, last bool) error {
		fmt.Fprintf(&b, "  %q: [", name)
		for i := range n {
			line, err := json.Marshal(entry(i))
			if err != nil {
				return err
			}
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("\n    ")
			b.Write(line)
		}
		if n > 0 {
			b.WriteString("\n  ")
		}
		b.WriteString("]")
		if !last {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
		return nil
	}
	if err := list("censuses", len(cat.Censuses), func(i int) any { return cat.Censuses[i] }, false); err != nil {
		return err
	}
	if err := list("fields", len(cat.Fields), func(i int) any { return cat.Fields[i] }, true); err != nil {
		return err
	}
	b.WriteString("}\n")
	return writeFile(path, b.Bytes())
}

// --- rendering -----------------------------------------------------------------------

var classLabel = map[string]string{
	"safe":         "safe",
	"prompt":       "prompt · withheld",
	"tool_content": "tool content · withheld",
	"unclassified": "unclassified",
	"":             "?",
}

func renderFields(cat Catalog, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Harness telemetry fields\n\n")
	b.WriteString("Generated by `make compat` (test/e2e/compatgen) from the live suite's field census: every attribute key each harness")
	b.WriteString(" build exported over OTLP while the suite drove it, on each surface (a log event, a span, its events and links, a metric and its exemplars,")
	b.WriteString(" the instrumentation scope, the resource), with the kinds of value it carried, and what terma's relay does with it when a project withholds content.")
	fmt.Fprintf(&b, " Last generated %s. Do not edit by hand: the catalog is `docs/compat/fields.json`, beside this file on the compat-matrix branch.\n\n", now.Format("2006-01-02"))
	b.WriteString("**safe** leaves whatever the policy · **prompt** / **tool content** leave only when the project collects them · ")
	b.WriteString("**unclassified** is withheld, and counted, until it is classified in `internal/relay` or an agent's capture rules: on a record whenever its value is not a number or a flag, on a resource whatever its value.\n\n")
	byHarness := map[string][]FieldEntry{}
	for _, f := range cat.Fields {
		byHarness[f.Harness] = append(byHarness[f.Harness], f)
	}
	b.WriteString("## At a glance\n\n| Harness | Newest build censused | Surfaces | Keys | Unclassified, withheld |\n|---|---|---|---|---|\n")
	for _, h := range e2e.Harnesses {
		shown, newest, ok := cat.shown(h.ID)
		if !ok {
			continue
		}
		keys, withheld := 0, 0
		for _, f := range byHarness[h.ID] {
			if f.sentBy(shown.Version) || (shown.Partial && f.seenBy(shown.Version)) {
				keys++
				if f.withheld() {
					withheld++
				}
			}
		}
		build := shown.Version
		if newest.id() != shown.id() {
			build += " (" + newest.Version + " partial)"
		} else if shown.Partial {
			build += " (partial)"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", h.Name, build, len(shown.Surfaces), keys, withheld)
	}
	for _, h := range e2e.Harnesses {
		shown, newest, ok := cat.shown(h.ID)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\nNewest build censused: %s.", h.Name, newest.Version)
		switch {
		case newest.id() != shown.id():
			fmt.Fprintf(&b, " Its census was partial (a census scenario failed), so the keys below are as %s, the newest censused whole, has them.", shown.Version)
		case shown.Partial:
			b.WriteString(" Its census was partial (a census scenario failed), and no build was censused whole: a key it did not see may still be sent.")
		}
		b.WriteString("\n")
		newest = shown
		bySurface := map[string][]FieldEntry{}
		for _, f := range byHarness[h.ID] {
			bySurface[f.Surface] = append(bySurface[f.Surface], f)
		}
		for _, s := range slices.Sorted(maps.Keys(bySurface)) {
			fmt.Fprintf(&b, "\n### `%s`\n\n| Key | Class | Values | First censused | In %s |\n|---|---|---|---|---|\n", tableCell(s), newest.Version)
			for _, f := range bySurface[s] {
				in := "yes"
				switch {
				case f.sentBy(newest.Version), shown.Partial && f.seenBy(newest.Version):
				case f.seenBy(newest.Version):
					in = "only in a failed run"
				default:
					in = "**no** (last " + f.Versions[0] + ")"
				}
				class := classLabel[f.Class]
				if f.withheld() {
					class = "**unclassified · withheld**"
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", tableCell(f.Key), class, strings.Join(f.Kinds, ", "), f.FirstSeen, in)
			}
		}
	}
	return b.String()
}

// tableCell is a key or surface as a markdown table cell holds it: a pipe would end the cell.
func tableCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
