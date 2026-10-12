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

type block = map[string]any

func mrkdwn(s string) block { return block{"type": "mrkdwn", "text": s} }

// slackText is s escaped for Slack, its links written as Slack writes them.
func slackText(s string) string { return renderLinks(slackEscape(s), true) }

// slackSection is s as a section block, cut to what one section holds.
func slackSection(s string) block {
	return block{"type": "section", "text": mrkdwn(clip(slackText(s), sectionLimit))}
}

// attachment is one bar of a harness's changes: its colour, "" for Slack's own, and its text.
type attachment struct{ colour, text string }

// slack is the digest as a Slack incoming webhook's payload: as blocks a summary (slackSummary),
// then, under a divider, each harness's changes as attachments (slackChanges), as many whole
// harnesses as one message holds.
func (d Drift) slack() map[string]any {
	summary := "Harness drift: no changes"
	if !d.Quiet() {
		summary = "Harness drift: changes to look at"
	}
	blocks := d.slackSummary()
	var harnesses [][]attachment
	for _, h := range d.Harnesses {
		if h.changed() {
			harnesses = append(harnesses, h.slackChanges())
		}
	}
	if len(harnesses) > 0 {
		blocks = append(blocks, block{"type": "divider"}, slackSection("*Field changes by harness*"))
	}
	var atts []attachment
	fit := fitHarnesses(blocks, harnesses)
	for _, a := range fit {
		atts = append(atts, a...)
	}
	if left := len(harnesses) - len(fit); left > 0 {
		more := "1 harness"
		if left > 1 {
			more = fmt.Sprintf("%d harnesses", left)
		}
		atts = append(atts, attachment{"", "_… and " + more + " more, too long for one message: see the run's summary._"})
	}
	payload := map[string]any{"text": summary, "blocks": blocks}
	if len(atts) > 0 {
		var out []block
		for _, a := range atts {
			att := block{"blocks": []block{slackSection(a.text)}}
			if a.colour != "" {
				att["color"] = a.colour
			}
			out = append(out, att)
		}
		payload["attachments"] = out
	}
	return payload
}

// slackSummary is the digest's summary as blocks: a header, the alarms, the new releases, the
// builds unchanged, what is still withheld, compatibility, and a link to the run.
func (d Drift) slackSummary() []block {
	blocks := []block{{"type": "header", "text": block{"type": "plain_text", "text": "Harness drift — " + d.GeneratedAt.Format("2006-01-02")}}}
	for _, a := range d.alarms() {
		blocks = append(blocks, slackSection(":warning: *"+a+"*"))
	}
	if len(d.Releases) == 0 {
		blocks = append(blocks, slackSection("*New releases:* none tonight"))
	} else {
		blocks = append(blocks, slackSection("*New releases*\n• "+strings.Join(d.releaseLines(func(s string) string { return "*" + s + "*" }), "\n• ")))
	}
	if d.Quiet() {
		blocks = append(blocks, slackSection("No changes. Censused: "+d.unchanged()+"."))
	} else if u := d.unchanged(); u != "" {
		blocks = append(blocks, slackSection("_Unchanged:_ "+u))
	}
	if s := d.stillWithheld(); s != "" {
		blocks = append(blocks, slackSection(s))
	}
	if len(d.SourceErrors) > 0 {
		blocks = append(blocks, slackSection("_Source links left out: "+strings.Join(d.SourceErrors, "; ")+"._"))
	}
	if compat := d.otherCompat(); len(compat) > 0 {
		var lines []string
		for _, c := range compat {
			lines = append(lines, c.line())
		}
		blocks = append(blocks, slackSection("*Compatibility changes*\n• "+strings.Join(listed(lines), "\n• ")))
	}
	if d.Link != "" {
		blocks = append(blocks, block{"type": "context", "elements": []block{mrkdwn("<" + d.Link + "|The run> · the catalog and the matrix are on the compat-matrix branch")}})
	}
	return blocks
}

// slackChanges are h's changes as attachments: its headline, with what neither came nor went
// (the newly withheld), on Slack's own bar, for a colour says only what came and what went,
// never how the build did; then what came on a green bar and what went on a red. One source
// link a finding, or none where the links would push a bar past what a section holds and cut
// its findings (drift.md has them all).
func (h HarnessDrift) slackChanges() []attachment {
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
			// An unsigned list has no colour (colours has no ""), so it joins the headline's bar.
			i := slices.IndexFunc(out, func(a attachment) bool { return a.colour == colours[sec.sign] })
			if i < 0 {
				out = append(out, attachment{colours[sec.sign], list})
				continue
			}
			out[i].text += "\n" + list
		}
		return out
	}
	out := said(oneLink)
	if slices.ContainsFunc(out, func(a attachment) bool { return len(slackText(a.text)) > sectionLimit }) {
		out = said(noLinks)
	}
	return out
}

// fitHarnesses are the first of harnesses, whole, that fit one message after blocks: at most
// maxBlocks blocks and attachments, one kept for the line saying what was left out, and their
// text within the budget. A harness is kept or left out whole, never its headline without
// what went.
func fitHarnesses(blocks []block, harnesses [][]attachment) [][]attachment {
	n, size := len(blocks), 0
	for _, b := range blocks {
		if t, ok := b["text"].(block); ok {
			size += len(t["text"].(string))
		}
	}
	for i, h := range harnesses {
		for _, a := range h {
			size += len(slackText(a.text))
		}
		if n += len(h); n > maxBlocks-1 || size > slackBudget {
			return harnesses[:i]
		}
	}
	return harnesses
}
