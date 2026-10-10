package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The field catalog (docs/compat/fields.json) is what terma knows each harness emits: every
// attribute key on every surface, the builds it was seen in, the kinds of value it carried,
// and what terma's relay does with it. Each run's census (report/fields.json) is merged into
// it; docs/FIELDS.md renders it.

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
	// Versions are the builds it was seen in, newest first.
	Versions []string  `json:"versions"`
	LastSeen time.Time `json:"last_seen"`
}

func (f FieldEntry) id() string { return f.Harness + "\x00" + f.Surface + "\x00" + f.Key }

func (c Census) id() string { return c.Harness + "\x00" + c.Version }

// mergeFields files a run's census in the catalog.
func mergeFields(cat *Catalog, rows []e2e.FieldRow) {
	fields := map[string]*FieldEntry{}
	for i := range cat.Fields {
		fields[cat.Fields[i].id()] = &cat.Fields[i]
	}
	censuses := map[string]*Census{}
	for i := range cat.Censuses {
		censuses[cat.Censuses[i].id()] = &cat.Censuses[i]
	}
	var added []*FieldEntry
	var newCensuses []Census
	seenSurfaces := map[string]map[string]bool{}
	for _, r := range rows {
		cid := r.Harness + "\x00" + r.Version
		if seenSurfaces[cid] == nil {
			seenSurfaces[cid] = map[string]bool{}
		}
		seenSurfaces[cid][r.Surface] = true
		e := FieldEntry{Harness: r.Harness, Surface: r.Surface, Key: r.Key}
		prev, ok := fields[e.id()]
		if !ok {
			fresh := &FieldEntry{Harness: r.Harness, Surface: r.Surface, Key: r.Key, Class: r.Class,
				Kinds: slices.Clone(r.Kinds), Versions: []string{r.Version}, LastSeen: r.At}
			added = append(added, fresh)
			fields[e.id()] = fresh
			continue
		}
		if !slices.Contains(prev.Versions, r.Version) {
			prev.Versions = append(prev.Versions, r.Version)
			sortVersionsDesc(prev.Versions)
		}
		for _, k := range r.Kinds {
			if !slices.Contains(prev.Kinds, k) {
				prev.Kinds = append(prev.Kinds, k)
			}
		}
		slices.Sort(prev.Kinds)
		if r.At.After(prev.LastSeen) {
			prev.LastSeen = r.At
			if r.Class != "" {
				prev.Class = r.Class
			}
		}
	}
	for cid, surfaces := range seenSurfaces {
		harness, version, _ := strings.Cut(cid, "\x00")
		var at time.Time
		for _, r := range rows {
			if r.Harness == harness && r.Version == version && r.At.After(at) {
				at = r.At
			}
		}
		c := Census{Harness: harness, Version: version, Surfaces: slices.Sorted(keysOf(surfaces)), At: at}
		if prev, ok := censuses[c.id()]; ok {
			prev.Surfaces = slices.Sorted(keysOf(union(prev.Surfaces, c.Surfaces)))
			if at.After(prev.At) {
				prev.At = at
			}
			continue
		}
		newCensuses = append(newCensuses, c)
	}
	for _, f := range added {
		cat.Fields = append(cat.Fields, *f)
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
}

func keysOf(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func union(a, b []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		m[s] = true
	}
	return m
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
		fs := byHarness[h.ID]
		newest, ok := cat.newestCensus(h.ID)
		if !ok || len(fs) == 0 {
			continue
		}
		surfaces := map[string]bool{}
		withheld := 0
		for _, f := range fs {
			if slices.Contains(f.Versions, newest.Version) {
				surfaces[f.Surface] = true
				if withheldUnclassified(f.Class, f.Kinds) {
					withheld++
				}
			}
		}
		keys := 0
		for _, f := range fs {
			if slices.Contains(f.Versions, newest.Version) {
				keys++
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", h.Name, newest.Version, len(surfaces), keys, withheld)
	}
	for _, h := range e2e.Harnesses {
		fs := byHarness[h.ID]
		newest, ok := cat.newestCensus(h.ID)
		if !ok || len(fs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\nNewest build censused: %s.\n", h.Name, newest.Version)
		bySurface := map[string][]FieldEntry{}
		for _, f := range fs {
			bySurface[f.Surface] = append(bySurface[f.Surface], f)
		}
		for _, s := range slices.Sorted(func(yield func(string) bool) {
			for k := range bySurface {
				if !yield(k) {
					return
				}
			}
		}) {
			fmt.Fprintf(&b, "\n### `%s`\n\n| Key | Class | Values | First censused | In %s |\n|---|---|---|---|---|\n", s, newest.Version)
			for _, f := range bySurface[s] {
				in := "yes"
				if !slices.Contains(f.Versions, newest.Version) {
					in = "**no** (last " + f.Versions[0] + ")"
				}
				class := classLabel[f.Class]
				if withheldUnclassified(f.Class, f.Kinds) {
					class = "**unclassified · withheld**"
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", f.Key, class, strings.Join(f.Kinds, ", "), f.Versions[len(f.Versions)-1], in)
			}
		}
	}
	return b.String()
}
