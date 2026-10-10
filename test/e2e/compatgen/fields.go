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
}

// FieldEntry is one key a harness emitted on one surface.
type FieldEntry struct {
	Harness string `json:"harness"`
	Surface string `json:"surface"`
	Key     string `json:"key"`
	// Class is what the relay of the latest run that saw it does with it.
	Class string   `json:"class"`
	Kinds []string `json:"kinds"`
	// FirstSeen is the oldest build the catalog saw it in; Versions the newest it was seen in,
	// newest first, at most keepVersions.
	FirstSeen string   `json:"first_seen"`
	Versions  []string `json:"versions"`
}

const (
	keepVersions = 5
	keepCensuses = 5
)

func (f FieldEntry) id() string { return f.Harness + "\x00" + f.Surface + "\x00" + f.Key }

func (c Census) id() string { return c.Harness + "\x00" + c.Version }

// mergeFields files a run's census in the catalog. Runs are merged in the order they ran, so
// a key's class is the latest run's.
func mergeFields(cat *Catalog, rows []e2e.FieldRow) {
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
			f = &FieldEntry{Harness: r.Harness, Surface: r.Surface, Key: r.Key, FirstSeen: r.Version, Kinds: slices.Clone(r.Kinds), Class: r.Class}
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
		// The kinds and class are the newest build's: a key an old build sent as text and a
		// new one as a number is no longer withheld.
		switch {
		case f.Versions[0] != r.Version:
		case newest != r.Version:
			f.Kinds, f.Class = slices.Clone(r.Kinds), r.Class
		default:
			for _, k := range r.Kinds {
				if !slices.Contains(f.Kinds, k) {
					f.Kinds = append(f.Kinds, k)
				}
			}
			slices.Sort(f.Kinds)
			if r.Class != "" {
				f.Class = r.Class
			}
		}
	}
	for _, f := range added {
		cat.Fields = append(cat.Fields, *f)
	}
	censuses := map[string]*Census{}
	for i := range cat.Censuses {
		censuses[cat.Censuses[i].id()] = &cat.Censuses[i]
	}
	for b, surfaces := range seen {
		c := Census{Harness: b.harness, Version: b.version, Surfaces: slices.Sorted(maps.Keys(surfaces)), At: at[b]}
		if prev, ok := censuses[c.id()]; ok {
			for _, s := range prev.Surfaces {
				surfaces[s] = true
			}
			prev.Surfaces = slices.Sorted(maps.Keys(surfaces))
			if c.At.After(prev.At) {
				prev.At = c.At
			}
			continue
		}
		cat.Censuses = append(cat.Censuses, c)
	}
	sort.Slice(cat.Fields, func(i, j int) bool { return cat.Fields[i].id() < cat.Fields[j].id() })
	sort.Slice(cat.Censuses, func(i, j int) bool {
		a, b := cat.Censuses[i], cat.Censuses[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return versionLess(b.Version, a.Version)
	})
	// A harness keeps its newest censuses, and a key only while one of them saw it.
	kept := cat.Censuses[:0]
	count := map[string]int{}
	censused := map[string]bool{} // harness and version
	for _, c := range cat.Censuses {
		if count[c.Harness] < keepCensuses {
			kept = append(kept, c)
			censused[c.id()] = true
		}
		count[c.Harness]++
	}
	cat.Censuses = kept
	cat.Fields = slices.DeleteFunc(cat.Fields, func(f FieldEntry) bool {
		return !slices.ContainsFunc(f.Versions, func(v string) bool { return censused[f.Harness+"\x00"+v] })
	})
}

func sortVersionsDesc(vs []string) {
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[j], vs[i]) })
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

// withheldUnclassified reports a key the relay drops as unclassified: one with a value that
// is text. An unclassified number or flag passes.
func withheldUnclassified(class string, kinds []string) bool {
	return class == "unclassified" && slices.ContainsFunc(kinds, func(k string) bool { return k != e2e.KindNumber && k != e2e.KindBool })
}

func renderFields(cat Catalog, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Harness telemetry fields\n\n")
	b.WriteString("Generated by `make compat` (test/e2e/compatgen) from the live suite's field census: every attribute key each harness")
	b.WriteString(" build exported over OTLP while the suite drove it, on each surface (a log event, a span or its event, a metric, the resource),")
	b.WriteString(" and what terma's relay does with it when a project withholds content.")
	fmt.Fprintf(&b, " Last generated %s. Do not edit by hand: the catalog is `docs/compat/fields.json`.\n\n", now.Format("2006-01-02"))
	b.WriteString("**safe** leaves whatever the policy · **prompt** / **tool content** leave only when the project collects them · ")
	b.WriteString("**unclassified** is withheld when its value is text, and counted, until it is classified in `internal/relay` or an agent's capture rules.\n\n")
	byHarness := map[string][]FieldEntry{}
	for _, f := range cat.Fields {
		byHarness[f.Harness] = append(byHarness[f.Harness], f)
	}
	b.WriteString("## At a glance\n\n| Harness | Newest build censused | Surfaces | Keys | Unclassified, withheld |\n|---|---|---|---|---|\n")
	for _, h := range e2e.Harnesses {
		newest, ok := cat.newestCensus(h.ID)
		if !ok {
			continue
		}
		keys, withheld := 0, 0
		for _, f := range byHarness[h.ID] {
			if slices.Contains(f.Versions, newest.Version) {
				keys++
				if withheldUnclassified(f.Class, f.Kinds) {
					withheld++
				}
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", h.Name, newest.Version, len(newest.Surfaces), keys, withheld)
	}
	for _, h := range e2e.Harnesses {
		newest, ok := cat.newestCensus(h.ID)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\nNewest build censused: %s.\n", h.Name, newest.Version)
		bySurface := map[string][]FieldEntry{}
		for _, f := range byHarness[h.ID] {
			bySurface[f.Surface] = append(bySurface[f.Surface], f)
		}
		for _, s := range slices.Sorted(maps.Keys(bySurface)) {
			fmt.Fprintf(&b, "\n### `%s`\n\n| Key | Class | Values | First censused | In %s |\n|---|---|---|---|---|\n", tableCell(s), newest.Version)
			for _, f := range bySurface[s] {
				in := "yes"
				if !slices.Contains(f.Versions, newest.Version) {
					in = "**no** (last " + f.Versions[0] + ")"
				}
				class := classLabel[f.Class]
				if withheldUnclassified(f.Class, f.Kinds) {
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
