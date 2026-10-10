package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The digest is what a run found changed against the catalog and the history as they were
// committed: per harness, the newest build it took the census of, the keys that appeared,
// the keys that went from a surface it still saw, and the keys the relay withholds as
// unclassified; and every capability whose result changed. report/drift.json holds it,
// report/drift.md and report/slack.json say it, the second for a Slack incoming webhook.

// Drift is the digest of one run.
type Drift struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Link        string         `json:"link,omitempty"`
	Harnesses   []HarnessDrift `json:"harnesses"`
	Compat      []CompatChange `json:"compat"`
}

// HarnessDrift is what changed for one harness.
type HarnessDrift struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	// Version is the newest build the run took the census of; Previous the newest the
	// catalog had, "" for a harness it had none of.
	Version  string `json:"version"`
	Previous string `json:"previous,omitempty"`
	// Added are keys no build of the harness had on their surface; Removed are keys
	// Previous had on a surface Version was seen on, without them.
	Added   []FieldChange `json:"added,omitempty"`
	Removed []FieldChange `json:"removed,omitempty"`
	// Unseen are surfaces Previous had that the run did not see on Version: gone, or not
	// reached this run.
	Unseen []string `json:"unseen,omitempty"`
	// Withheld are Version's unclassified keys with text values, which the relay drops, that
	// the catalog did not have withheld; StillWithheld those it did: until each is classified.
	Withheld      []FieldChange `json:"withheld,omitempty"`
	StillWithheld []FieldChange `json:"still_withheld,omitempty"`
}

// changed reports whether h has anything new to say.
func (h HarnessDrift) changed() bool { return len(h.Added)+len(h.Removed)+len(h.Withheld) > 0 }

// FieldChange is one key on one surface.
type FieldChange struct {
	Surface string   `json:"surface"`
	Key     string   `json:"key"`
	Class   string   `json:"class,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
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

// Quiet reports a run that changed nothing worth a look.
func (d Drift) Quiet() bool {
	for _, h := range d.Harnesses {
		if h.changed() {
			return false
		}
	}
	return len(d.Compat) == 0
}

// fieldDrift compares a run's census with the catalog before it is merged.
func fieldDrift(cat Catalog, rows []e2e.FieldRow) []HarnessDrift {
	byHarness := map[string][]e2e.FieldRow{}
	for _, r := range rows {
		byHarness[r.Harness] = append(byHarness[r.Harness], r)
	}
	known := map[string]FieldEntry{}
	for _, f := range cat.Fields {
		known[f.id()] = f
	}
	var out []HarnessDrift
	for _, harness := range slices.Sorted(func(yield func(string) bool) {
		for h := range byHarness {
			if !yield(h) {
				return
			}
		}
	}) {
		hr := byHarness[harness]
		version := hr[0].Version
		for _, r := range hr {
			if versionLess(version, r.Version) {
				version = r.Version
			}
		}
		d := HarnessDrift{Harness: harness, Name: harnessName(harness), Version: version}
		now := map[string]e2e.FieldRow{}
		surfaces := map[string]bool{}
		for _, r := range hr {
			if r.Version != version {
				continue
			}
			now[r.Surface+"\x00"+r.Key] = r
			surfaces[r.Surface] = true
		}
		for _, id := range sortedKeys(now) {
			r := now[id]
			if _, ok := known[FieldEntry{Harness: harness, Surface: r.Surface, Key: r.Key}.id()]; !ok {
				d.Added = append(d.Added, FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds})
			}
			if withheldUnclassified(r.Class, r.Kinds) {
				c := FieldChange{Surface: r.Surface, Key: r.Key, Class: r.Class, Kinds: r.Kinds}
				if prev, ok := known[FieldEntry{Harness: harness, Surface: r.Surface, Key: r.Key}.id()]; ok && withheldUnclassified(prev.Class, prev.Kinds) {
					d.StillWithheld = append(d.StillWithheld, c)
				} else {
					d.Withheld = append(d.Withheld, c)
				}
			}
		}
		if prev, ok := cat.newestCensus(harness); ok && !versionLess(version, prev.Version) {
			d.Previous = prev.Version
			for _, f := range cat.Fields {
				if f.Harness != harness || !slices.Contains(f.Versions, prev.Version) || !surfaces[f.Surface] {
					continue
				}
				if _, ok := now[f.Surface+"\x00"+f.Key]; !ok {
					d.Removed = append(d.Removed, FieldChange{Surface: f.Surface, Key: f.Key, Class: f.Class, Kinds: f.Kinds})
				}
			}
			for _, s := range prev.Surfaces {
				if !surfaces[s] {
					d.Unseen = append(d.Unseen, s)
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// compatDrift compares a run's results with the history before they are merged.
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	build := h.Version
	switch {
	case h.Previous == "":
		build += " (first census)"
	case h.Previous != h.Version:
		build += " (was " + h.Previous + ")"
	}
	var parts []string
	if n := len(h.Added); n > 0 {
		parts = append(parts, plural(n, "new field"))
	}
	if n := len(h.Removed); n > 0 {
		parts = append(parts, plural(n, "removed field"))
	}
	if n := len(h.Withheld); n > 0 {
		parts = append(parts, plural(n, "newly withheld field"))
	}
	if n := len(h.Unseen); n > 0 {
		parts = append(parts, plural(n, "surface")+" not seen")
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

func changeLines(cs []FieldChange, withClass bool) []string {
	var out []string
	for i, c := range cs {
		if i == maxListed {
			out = append(out, fmt.Sprintf("… and %d more", len(cs)-maxListed))
			break
		}
		line := "`" + c.Key + "` on `" + c.Surface + "`"
		if withClass && c.Class != "" {
			line += " (" + classLabel[c.Class] + ")"
		}
		out = append(out, line)
	}
	return out
}

func (c CompatChange) line() string {
	from := c.From
	if from == "" {
		from = "new build"
	}
	return fmt.Sprintf("%s %s · %s · %s: %s → %s", harnessName(c.Harness), c.Version, platformShort(c.Platform), capabilityLabel(c.Capability), from, c.To)
}

// unchanged names the harness builds with nothing new, as one line; a build the catalog had
// not seen says so, its fields unchanged or not.
func (d Drift) unchanged() string {
	var builds []string
	for _, h := range d.Harnesses {
		if h.changed() {
			continue
		}
		b := h.Name + " " + h.Version
		switch {
		case h.Previous == "":
			b += " (first census)"
		case h.Previous != h.Version:
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
		if n := len(h.StillWithheld); n > 0 {
			keys := map[string]bool{}
			for _, c := range h.StillWithheld {
				keys["`"+c.Key+"`"] = true
			}
			parts = append(parts, fmt.Sprintf("%s: %s", h.Name, strings.Join(sortedKeys(keys), ", ")))
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
		if !h.changed() && len(h.Unseen) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", h.headline())
		for _, sec := range []struct {
			title string
			lines []string
		}{
			{"New fields", changeLines(h.Added, true)},
			{"Removed fields", changeLines(h.Removed, true)},
			{"Newly withheld by the relay, unclassified", changeLines(h.Withheld, false)},
			{"Surfaces not seen", wrapCode(h.Unseen)},
		} {
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

func wrapCode(ss []string) []string {
	var out []string
	for i, s := range ss {
		if i == maxListed {
			out = append(out, fmt.Sprintf("… and %d more", len(ss)-maxListed))
			break
		}
		out = append(out, "`"+s+"`")
	}
	return out
}

// slack is the digest as a Slack incoming webhook's payload: a header, a section per harness
// and one for compatibility, and a link to the run.
func (d Drift) slack() map[string]any {
	type block = map[string]any
	text := func(s string) block { return block{"type": "mrkdwn", "text": s} }
	section := func(s string) block {
		if len(s) > 2900 {
			s = s[:2900] + "…"
		}
		return block{"type": "section", "text": text(s)}
	}
	summary := "Harness drift: no changes"
	if !d.Quiet() {
		summary = "Harness drift: changes to look at"
	}
	blocks := []block{{"type": "header", "text": block{"type": "plain_text", "text": "Harness drift — " + d.GeneratedAt.Format("2006-01-02")}}}
	if d.Quiet() {
		blocks = append(blocks, section("No changes. Censused: "+d.unchanged()+"."))
	}
	for _, h := range d.Harnesses {
		if !h.changed() {
			continue
		}
		var b strings.Builder
		b.WriteString("*" + h.headline() + "*")
		for _, sec := range []struct {
			title string
			lines []string
		}{
			{"New", changeLines(h.Added, true)},
			{"Removed", changeLines(h.Removed, true)},
			{"Newly withheld (unclassified)", changeLines(h.Withheld, false)},
		} {
			if len(sec.lines) == 0 {
				continue
			}
			b.WriteString("\n_" + sec.title + "_\n• " + strings.Join(sec.lines, "\n• "))
		}
		blocks = append(blocks, section(b.String()))
	}
	if !d.Quiet() {
		if u := d.unchanged(); u != "" {
			blocks = append(blocks, section("_Unchanged:_ "+u))
		}
	}
	if s := d.stillWithheld(); s != "" {
		blocks = append(blocks, section(s))
	}
	if len(d.Compat) > 0 {
		var lines []string
		for i, c := range d.Compat {
			if i == maxListed {
				lines = append(lines, fmt.Sprintf("… and %d more", len(d.Compat)-maxListed))
				break
			}
			lines = append(lines, c.line())
		}
		blocks = append(blocks, section("*Compatibility changes*\n• "+strings.Join(lines, "\n• ")))
	}
	if d.Link != "" {
		blocks = append(blocks, block{"type": "context", "elements": []block{text("<" + d.Link + "|The run> · accept a change with `make compat` in test/e2e")}})
	}
	return map[string]any{"text": summary, "blocks": blocks}
}
