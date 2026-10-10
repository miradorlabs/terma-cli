package main

import (
	"fmt"
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
