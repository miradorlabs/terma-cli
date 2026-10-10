package main

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Saying the digest: report/drift.md for the run's summary, and report/slack.json for a Slack
// incoming webhook.

// maxListed bounds each list a message spells out; the rest is counted.
const maxListed = 12

func (h HarnessDrift) headline() string {
	if h.First {
		build := h.Version
		if h.Unreached != "" {
			build += " (" + h.Unreached + ", run tonight, was not reached)"
		}
		return fmt.Sprintf("%s %s: first census, %s, %s", h.Name, build, plural(h.Surfaces, "surface"), plural(h.Keys, "key"))
	}
	build := h.build("")
	var parts []string
	// Each says its plural, which is not always its last word's.
	for _, p := range []struct {
		n         int
		one, many string
	}{
		{len(h.NewSurfaces), "new surface", "new surfaces"}, {len(h.Added), "new field", "new fields"},
		{len(h.Removed), "removed field", "removed fields"},
		{len(h.Unseen), "surface no longer sent", "surfaces no longer sent"},
		{len(h.Withheld), "newly withheld field", "newly withheld fields"},
	} {
		switch {
		case p.n == 1:
			parts = append(parts, "1 "+p.one)
		case p.n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.many))
		}
	}
	switch {
	case h.Partial && h.Judged != "":
		parts = append(parts, "census partial (a scenario failed), removals judged on "+h.Judged)
	case h.Partial:
		parts = append(parts, "census partial (a scenario failed), nothing judged removed")
	}
	if len(parts) == 0 {
		parts = append(parts, "no field changes")
	}
	return h.Name + " " + build + ": " + strings.Join(parts, ", ")
}

// build is the build a harness's line names, and what it was compared with: the build before
// it (newBuild says it is one, on a line of builds with nothing new), the build not reached,
// the build what is gone was judged against, and the comparison of their source.
func (h HarnessDrift) build(newBuild string) string {
	var notes []string
	switch {
	case h.Unreached == h.Previous && h.Unreached != "":
		notes = append(notes, h.Unreached+", the newest censused before, was not reached")
	case h.Unreached != "":
		notes = append(notes, h.Unreached+", run tonight, was not reached")
	case h.Previous != h.Version:
		notes = append(notes, newBuild+"was "+h.Previous)
	}
	switch {
	case h.Earlier != "":
		notes = append(notes, "judged against "+h.Earlier+"'s earlier nights, its census tonight partial")
	case h.Since != "":
		notes = append(notes, "judged against "+h.Since+", censused whole")
	}
	if len(notes) == 0 {
		return h.Version
	}
	return h.Version + " (" + strings.Join(notes, "; ") + h.compareLink() + ")"
}

// compareLink links the source's changes since Previous, where they are public.
func (h HarnessDrift) compareLink() string {
	if h.Compare == "" {
		return ""
	}
	return ", " + link(h.Compare, "diff")
}

// link is a link in a line of the digest, written as each format writes one (renderLinks):
// [text](url) in markdown, <url|text> in Slack once the text around it is escaped.
func link(url, text string) string { return "\x00" + url + "\x01" + text + "\x02" }

var linkMark = regexp.MustCompile("\x00([^\x01]*)\x01([^\x02]*)\x02")

// renderLinks writes the links in s as markdown (or Slack, with slack set) writes them.
func renderLinks(s string, slack bool) string {
	if slack {
		return linkMark.ReplaceAllString(s, "<$1|$2>")
	}
	return linkMark.ReplaceAllString(s, "[$2]($1)")
}

// detail is how much of a source note a format has room for: every link, one link a
// finding, or the note's words alone.
type detail int

const (
	allLinks detail = iota
	oneLink
	noLinks
)

// note is where a harness's source names a finding: its lines, or for one gone, whether the
// new build's source still names it, or where the build before did.
func (s *SourceSays) note(d detail) string {
	if s == nil {
		return ""
	}
	refs := func(rs []SourceRef, more int) string {
		if d == noLinks {
			return ""
		}
		if d == oneLink && len(rs) > 1 {
			rs, more = rs[:1], more+len(rs)-1
		}
		var out []string
		for _, r := range rs {
			out = append(out, link(r.URL, fmt.Sprintf("%s:%d", path.Base(r.Path), r.Line)))
		}
		if more > 0 {
			out = append(out, fmt.Sprintf("+%d more", more))
		}
		return strings.Join(out, ", ")
	}
	says := func(verdict string, rs []SourceRef, more int) string {
		if r := refs(rs, more); r != "" {
			return " · " + verdict + ": " + r
		}
		return " · " + verdict
	}
	switch {
	case !s.Gone:
		if r := refs(s.Refs, s.More); r != "" {
			return " · " + r
		}
		return ""
	case len(s.Refs) > 0:
		return says("still in "+s.Version+"'s source", s.Refs, s.More)
	case len(s.Before) > 0:
		return says("gone from "+s.Version+"'s source, was in "+s.Previous+"'s", s.Before, s.BeforeMore)
	case s.Previous == "":
		return " · not named in " + s.Version + "'s source"
	}
	return " · named in neither build's source"
}

// goneIn says the build a removal went in, where it is not the last judged, and the build
// that sends it again, if one does.
func goneIn(in, back string) string {
	switch {
	case in == "":
		return ""
	case back != "":
		return " (gone in " + in + ", back in " + back + ")"
	}
	return " (gone in " + in + ")"
}

// plural counts n of a noun that takes an "s".
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

func changeLines(cs []FieldChange, withClass bool, d detail) []string {
	var out []string
	for _, c := range cs {
		line := "`" + c.Key + "` on `" + c.Surface + "`"
		if withClass && c.Class != "" {
			line += " (" + classLabel[c.Class] + ")"
		}
		if c.From != "" {
			line += " (in " + c.From + ")"
		}
		line += goneIn(c.In, c.Back)
		out = append(out, line+c.Source.note(d))
	}
	return listed(out)
}

func surfaceLines(ss []SurfaceChange, d detail) []string {
	var out []string
	for _, s := range ss {
		in := ""
		if s.From != "" {
			in = ", in " + s.From
		}
		out = append(out, fmt.Sprintf("`%s` (%s, %d new%s)%s", s.Surface, plural(s.Keys, "key"), s.NewKeys, in, s.Source.note(d)))
	}
	return listed(out)
}

func goneLines(ss []GoneSurface, d detail) []string {
	var out []string
	for _, s := range ss {
		line := "`" + s.Surface + "`"
		line += goneIn(s.In, s.Back)
		out = append(out, line+s.Source.note(d))
	}
	return listed(out)
}

// sections are what a harness's change says, titled, with as much of each source note as d.
func (h HarnessDrift) sections(d detail) []struct {
	title string
	lines []string
} {
	return []struct {
		title string
		lines []string
	}{
		{"New surfaces", surfaceLines(h.NewSurfaces, d)},
		{"New fields", changeLines(h.Added, true, d)},
		{"Removed fields", changeLines(h.Removed, true, d)},
		{"Surfaces no longer sent", goneLines(h.Unseen, d)},
		{"Newly withheld by the relay, unclassified", changeLines(h.Withheld, false, d)},
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
		out = append(out, "No census this night of "+strings.Join(d.Missing, ", ")+": censused within the week or run tonight, its tests did not run or failed before the census, or its exporter sent nothing.")
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
		builds = append(builds, h.Name+" "+h.build("new build, "))
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
		for _, sec := range h.sections(allLinks) {
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
	for _, e := range d.SourceErrors {
		b.WriteString("_Source links left out: " + e + "._\n\n")
	}
	if d.Link != "" {
		fmt.Fprintf(&b, "[The run](%s)\n", d.Link)
	}
	return renderLinks(b.String(), false)
}

// A Slack message holds at most 50 blocks, and a section's text at most 3000 characters.
// sectionLimit keeps each section under that, and slackBudget the sections' text in all, so
// the payload of a night that changed everything stays a message Slack takes.
const (
	sectionLimit = 2900
	maxBlocks    = 50
	slackBudget  = 30000
)

// clip cuts s, escaped for Slack, to at most limit bytes at the end of a line, so neither a
// character, a code span nor an escape is split, and says so.
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
		// Every "&" in escaped text begins an escape and every "<" a link: one not closed
		// before the cut is cut off.
		if amp := strings.LastIndexByte(s[:cut], '&'); amp >= 0 && !strings.Contains(s[amp:cut], ";") {
			cut = amp
		}
		if lt := strings.LastIndexByte(s[:cut], '<'); lt >= 0 && !strings.Contains(s[lt:cut], ">") {
			cut = lt
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
	slackText := func(s string) string { return renderLinks(slackEscape(s), true) }
	section := func(s string) block {
		return block{"type": "section", "text": text(clip(slackText(s), sectionLimit))}
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
		// One source link a finding; none, where the links would push the section past its
		// limit and cut the findings at its end (drift.md has them all).
		said := func(level detail) string {
			var b strings.Builder
			b.WriteString("*" + h.headline() + "*")
			for _, sec := range h.sections(level) {
				if len(sec.lines) > 0 {
					b.WriteString("\n_" + sec.title + "_\n• " + strings.Join(sec.lines, "\n• "))
				}
			}
			return b.String()
		}
		s := said(oneLink)
		if len(slackText(s)) > sectionLimit {
			s = said(noLinks)
		}
		blocks = append(blocks, section(s))
	}
	if u := d.unchanged(); u != "" && !d.Quiet() {
		blocks = append(blocks, section("_Unchanged:_ "+u))
	}
	if s := d.stillWithheld(); s != "" {
		blocks = append(blocks, section(s))
	}
	if len(d.SourceErrors) > 0 {
		blocks = append(blocks, section("_Source links left out: "+strings.Join(d.SourceErrors, "; ")+"._"))
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
