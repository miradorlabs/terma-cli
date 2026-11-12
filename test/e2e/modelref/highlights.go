package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a vendor says is new in a model, in a few lines: the headings of its "what's new" page's
// breaking changes or new features (Anthropic's, Google's), or else the list that page leads
// with; for a vendor with no such page, the first sentence of each changelog entry that names
// the model. The vendor's words, cut, never rewritten.

var reNewsSection = regexp.MustCompile(`(?i)breaking|what[’']?s new|new in|important details|changes`)

// maxHighlight is how long a highlight may be before it is cut at a word.
const maxHighlight = 160

func vendorHighlights(page *Page, entries []ChangelogEntry, n int) []string {
	out := []string{}
	add := func(s string) {
		s = shorten(plain(s))
		if s != "" && len(out) < n {
			for _, o := range out {
				if strings.EqualFold(o, s) {
					return
				}
			}
			out = append(out, s)
		}
	}
	if page != nil {
		for _, h := range newsItems(page.Text) {
			add(h)
		}
		if len(out) == 0 {
			for _, b := range leadList(page.Text) {
				add(b)
			}
		}
	}
	if len(out) == 0 {
		for _, e := range entries {
			add(firstSentence(e.Text))
		}
	}
	return out
}

// newsItems are what a page's sections about what is new or breaking list, in order: each
// section's subheadings, or, where it has none, its list's items, each by its bold lead
// ("**Long decode:** …") or its first sentence. The page's own title is no such section.
func newsItems(text string) []string {
	var out []string
	in := 0 // the level of the news section being read, 0 outside one
	var heads, items []string
	flush := func() {
		if len(heads) > 0 {
			out = append(out, heads...)
		} else {
			out = append(out, items...)
		}
		heads, items = nil, nil
	}
	for l := range strings.SplitSeq(text, "\n") {
		level, h := heading(l)
		if level == 0 {
			t := strings.TrimSpace(l)
			if in > 0 && (strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "- ")) && !strings.HasPrefix(l, " ") {
				items = append(items, lead(t[2:]))
			}
			continue
		}
		switch {
		case in > 0 && level > in:
			heads = append(heads, h)
		case level >= 2 && reNewsSection.MatchString(h):
			flush()
			in = level
		default:
			flush()
			in = 0
		}
	}
	flush()
	return out
}

var reBoldLead = regexp.MustCompile(`^\*\*([^*]+?):?\*\*`)

// lead is a list item's bold label where it starts with one, else its first sentence.
func lead(item string) string {
	if m := reBoldLead.FindStringSubmatch(item); m != nil {
		return strings.TrimSuffix(m[1], ":")
	}
	return firstSentence(item)
}

// leadList is the first list that follows a line about changes or what is new: Anthropic's
// "Four changes from Claude Fable 5.1 can break your code:".
func leadList(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasSuffix(t, ":") || !regexp.MustCompile(`(?i)\b(change|changes|new|break)\b`).MatchString(t) {
			continue
		}
		var items []string
		for _, m := range lines[i+1:] {
			mt := strings.TrimSpace(m)
			switch {
			case strings.HasPrefix(mt, "* ") || strings.HasPrefix(mt, "- "):
				items = append(items, mt[2:])
			case mt == "" && len(items) == 0:
			default:
				if len(items) > 0 {
					return items
				}
			}
		}
		if len(items) > 0 {
			return items
		}
	}
	return nil
}

// firstSentence is the first sentence of a changelog entry or list item. A line that only heads
// the entry (an xAI note's title, OpenAI's "Feature · Model: … · API: …" tags) is not it, where
// the entry has more.
func firstSentence(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for len(lines) > 1 {
		first := strings.TrimSpace(lines[0])
		if lv, _ := heading(first); lv == 0 && !strings.Contains(first, " · ") && first != "" {
			break
		}
		lines = lines[1:]
	}
	s = plain(strings.TrimLeft(strings.TrimSpace(strings.Join(lines, " ")), "*-# "))
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == '.' || s[i] == '!') && s[i+1] == ' ' && (i == 0 || !isDigit(s[i-1])) {
			return s[:i+1]
		}
	}
	return s
}

// shorten cuts s at a word to at most maxHighlight characters.
func shorten(s string) string {
	if utf8.RuneCountInString(s) <= maxHighlight {
		return s
	}
	r := []rune(s)[:maxHighlight]
	cut := string(r)
	if i := strings.LastIndex(cut, " "); i > maxHighlight/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, ",;: ") + "…"
}
