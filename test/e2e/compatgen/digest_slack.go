package main

import (
	"fmt"
	"slices"
	"strings"
)

// The digest as a Slack incoming webhook's payload (report/slack.json).

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

// Slack colours no text, so what came and what went sit in attachments whose bar is green
// and red. Slack shows a message's attachments after its blocks, so the digest is a summary
// in blocks, then below a divider each harness's changes in attachments; the bar of one that
// is neither is Slack's own.
const (
	addedColour   = "#2EB67D"
	removedColour = "#E01E5A"
)

var colours = map[string]string{"+": addedColour, "-": removedColour}

// slack is the digest as a Slack incoming webhook's payload: as blocks a header, the alarms,
// the new releases, the builds unchanged, what is still withheld, compatibility and a link to
// the run; then, under a divider, each harness's changes as attachments, its headline on
// Slack's own bar, what came in green and what went in red.
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
	if len(d.Releases) == 0 {
		blocks = append(blocks, section("*New releases:* none tonight"))
	} else {
		blocks = append(blocks, section("*New releases*\n• "+strings.Join(d.releaseLines(func(s string) string { return "*" + s + "*" }), "\n• ")))
	}
	if d.Quiet() {
		blocks = append(blocks, section("No changes. Censused: "+d.unchanged()+"."))
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
	if compat := d.otherCompat(); len(compat) > 0 {
		var lines []string
		for _, c := range compat {
			lines = append(lines, c.line())
		}
		blocks = append(blocks, section("*Compatibility changes*\n• "+strings.Join(listed(lines), "\n• ")))
	}
	if d.Link != "" {
		blocks = append(blocks, block{"type": "context", "elements": []block{text("<" + d.Link + "|The run> · the catalog and the matrix are on the compat-matrix branch")}})
	}
	type attachment struct {
		colour string
		text   string
	}
	var atts []attachment
	for _, h := range d.Harnesses {
		if !h.changed() {
			continue
		}
		// The harness's headline on Slack's own bar, for a colour says only what came and what
		// went, never how the build did; then its lists, what came together and what went
		// together. One source link a finding, or none where the links would push a list past
		// its limit and cut its findings (drift.md has them all).
		said := func(level detail) []attachment {
			out := []attachment{{"", "*" + h.headline() + "*"}}
			for _, sec := range h.sections(level) {
				if len(sec.lines) == 0 {
					continue
				}
				bullet := "• "
				if sec.sign != "" {
					bullet = ""
				}
				list := "_" + sec.title + "_\n" + bullet + strings.Join(sec.lines, "\n"+bullet)
				if n := len(out); n > 0 && out[n-1].colour == colours[sec.sign] {
					out[n-1].text += "\n" + list
					continue
				}
				out = append(out, attachment{colours[sec.sign], list})
			}
			return out
		}
		lists := said(oneLink)
		if slices.ContainsFunc(lists, func(a attachment) bool { return len(slackText(a.text)) > sectionLimit }) {
			lists = said(noLinks)
		}
		atts = append(atts, lists...)
	}
	if len(atts) > 0 {
		blocks = append(blocks, block{"type": "divider"}, section("*Field changes by harness*"))
	}
	// The summary comes first, so what the budget leaves out is the harnesses at the end;
	// the run's summary has the whole digest.
	kept, size := len(blocks), 0
	for _, b := range blocks {
		if t, ok := b["text"].(block); ok {
			size += len(t["text"].(string))
		}
	}
	keptAtts := 0
	for _, a := range atts {
		n := len(slackText(a.text))
		// One place is kept for the line saying what was left out.
		if kept+keptAtts >= maxBlocks-1 || size+n > slackBudget {
			break
		}
		keptAtts, size = keptAtts+1, size+n
	}
	if left := len(atts) - keptAtts; left > 0 {
		atts = append(atts[:keptAtts], attachment{"", fmt.Sprintf("_… and %s more, too long for one message: see the run's summary._", plural(left, "section"))})
	}
	var out []block
	for _, a := range atts {
		att := block{"blocks": []block{section(a.text)}}
		if a.colour != "" {
			att["color"] = a.colour
		}
		out = append(out, att)
	}
	payload := map[string]any{"text": summary, "blocks": blocks}
	if len(out) > 0 {
		payload["attachments"] = out
	}
	return payload
}
