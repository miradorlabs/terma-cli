package main

import (
	"context"
	"strings"
	"time"
)

// A vendor's API changelog, cut into entries: each list item under a dated heading (or, where
// a date's text is no list, the whole of it), with the date it is under. A model's notes are
// the entries that name it, by its id or its name.

// dateState is what reading a changelog's headings in order knows: the year a heading without
// one is in, and the month the heading before named.
type dateState struct {
	now       time.Time
	year      int
	lastMonth int
}

// changelogEntries reads a vendor's changelog into entries, newest first as it lists them.
func changelogEntries(ctx context.Context, c *client, v *vendor, now time.Time) ([]ChangelogEntry, error) {
	text, err := c.text(ctx, v.changelogMD)
	if err != nil {
		return nil, err
	}
	return splitChangelog(text, v, now), nil
}

func splitChangelog(text string, v *vendor, now time.Time) []ChangelogEntry {
	st := &dateState{now: now}
	var out []ChangelogEntry
	date := ""
	dateLevel := 0
	var body []string
	flush := func() {
		if date != "" {
			for _, item := range changelogItems(body) {
				out = append(out, ChangelogEntry{Date: date, URL: v.changelogURL, Text: item})
			}
		}
		body = nil
	}
	for l := range strings.SplitSeq(text, "\n") {
		if level, h := heading(l); level > 0 {
			if d := v.changelogDate(h, level, st); d != "" {
				flush()
				date, dateLevel = d, level
				continue
			}
			if date != "" && level <= dateLevel {
				// Another heading at the dates' level or above (a month's) closes the date.
				flush()
				date = ""
				continue
			}
		}
		body = append(body, l)
	}
	flush()
	return out
}

// changelogItems are a date's entries: its list items, each with the lines that continue it, or
// its sections' text where it has no list (xAI's notes are a heading and a paragraph each).
func changelogItems(lines []string) []string {
	var items []string
	var cur []string
	inList := false
	flush := func() {
		if t := strings.TrimSpace(strings.Join(cur, "\n")); t != "" {
			items = append(items, t)
		}
		cur = nil
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "- "):
			if !strings.HasPrefix(l, " ") || !inList {
				flush()
			}
			inList = true
			cur = append(cur, l)
		case strings.HasPrefix(t, "#"):
			flush()
			inList = false
			cur = append(cur, l)
		case t == "" && inList:
			cur = append(cur, l)
		default:
			cur = append(cur, l)
		}
	}
	flush()
	return items
}

// mentions are the entries that name the model.
func mentions(entries []ChangelogEntry, id, name string) []ChangelogEntry {
	out := []ChangelogEntry{}
	for _, e := range entries {
		if names(e.Text, id, name) {
			out = append(out, e)
		}
	}
	return out
}
