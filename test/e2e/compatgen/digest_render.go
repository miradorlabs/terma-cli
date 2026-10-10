package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Saying the digest: report/drift.md for the run's summary, and report/slack.json for a Slack
// incoming webhook.

// maxListed bounds each list a message spells out; the rest is counted.
const maxListed = 12

func (h HarnessDrift) headline() string {
	if h.First {
		return fmt.Sprintf("%s %s: first census, %s, %s", h.Name, h.Version, plural(h.Surfaces, "surface"), plural(h.Keys, "key"))
	}
	build := h.Version
	switch {
	case h.Behind:
		build += " (" + h.Previous + ", the newest censused before, was not reached)"
	case h.Previous != h.Version:
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
		parts = append(parts, "no field changes")
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

// A Slack message holds at most 50 blocks, and a section's text at most 3000 characters.
// sectionLimit keeps each section under that, and slackBudget the sections' text in all, so
// the payload of a night that changed everything stays a message Slack takes.
const (
	sectionLimit = 2900
	maxBlocks    = 50
	slackBudget  = 30000
)

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

// slackEscape escapes what Slack's mrkdwn reads as markup in text that came from a harness:
// a key or surface with "<" in it would otherwise be a link.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slack is the digest as a Slack incoming webhook's payload: a header, the alarms, a section
// per harness that changed and one for compatibility, and a link to the run.
func (d Drift) slack() map[string]any {
	type block = map[string]any
	text := func(s string) block { return block{"type": "mrkdwn", "text": s} }
	section := func(s string) block {
		return block{"type": "section", "text": text(clip(slackEscape(s), sectionLimit))}
	}
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
	// The alarms and harnesses come first, so what the budget leaves out is the tail; the
	// run's summary has the whole digest.
	kept, size := 1, 0
	for _, b := range blocks[1:] {
		n := len(b["text"].(block)["text"].(string))
		if kept >= maxBlocks-2 || size+n > slackBudget {
			break
		}
		kept, size = kept+1, size+n
	}
	if left := len(blocks) - kept; left > 0 {
		blocks = append(blocks[:kept], section(fmt.Sprintf("_… and %s more, too long for one message: see the run's summary._", plural(left, "section"))))
	}
	if d.Link != "" {
		blocks = append(blocks, block{"type": "context", "elements": []block{text("<" + d.Link + "|The run> · the catalog and the matrix are on the compat-matrix branch")}})
	}
	return map[string]any{"text": summary, "blocks": blocks}
}
