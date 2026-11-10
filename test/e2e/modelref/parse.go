package main

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Reading vendors' Markdown: token counts as they write them ("1M", "128K", "1,050,000"),
// dates ("November 9, 2026", "Nov 9, 2026", "Jun 2026"), tables, lists, and the model a
// changelog entry names.

// tokenCount is n in "1M", "128K", "500k", "1,050,000", "65,536 tokens": a whole number of
// tokens; ok false where s has none.
func tokenCount(s string) (int64, bool) {
	m := reTokens.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	num := strings.ReplaceAll(m[1], ",", "")
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToUpper(m[2]) {
	case "K":
		f *= 1_000
	case "M":
		f *= 1_000_000
	}
	return int64(math.Round(f)), true
}

var reTokens = regexp.MustCompile(`(\d[\d,]*(?:\.\d+)?)\s*([KkMm])?\b`)

var months = map[string]time.Month{}

func init() {
	for m := time.January; m <= time.December; m++ {
		name := strings.ToLower(m.String())
		months[name] = m
		months[name[:3]] = m
	}
	months["sept"] = time.September
}

// isoDate is s, a date as a vendor writes it, as YYYY-MM-DD, or YYYY-MM where s names no day;
// "" where s is no date.
func isoDate(s string) string {
	s = strings.TrimSpace(s)
	if m := reISODate.FindString(s); m != "" {
		return m
	}
	if m := reMonthDayYear.FindStringSubmatch(s); m != nil {
		if mo, ok := months[strings.ToLower(strings.TrimSuffix(m[1], "."))]; ok {
			d, _ := strconv.Atoi(m[2])
			y, _ := strconv.Atoi(m[3])
			return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		}
	}
	if m := reMonthYear.FindStringSubmatch(s); m != nil {
		if mo, ok := months[strings.ToLower(strings.TrimSuffix(m[1], "."))]; ok {
			y, _ := strconv.Atoi(m[2])
			return time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
		}
	}
	return ""
}

var (
	reISODate      = regexp.MustCompile(`\b\d{4}-\d{2}(?:-\d{2})?\b`)
	reMonthDayYear = regexp.MustCompile(`\b([A-Za-z]{3,9}\.?)\s+(\d{1,2}),?\s+(\d{4})\b`)
	reMonthYear    = regexp.MustCompile(`\b([A-Za-z]{3,9}\.?)\s+(\d{4})\b`)
)

// table is a Markdown table's rows, each its cells trimmed, the header row first and the
// separator row left out.
func table(lines []string) [][]string {
	var rows [][]string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if reSeparator.MatchString(l) {
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

var reSeparator = regexp.MustCompile(`^\|[\s:|-]+\|$`)

// tables are every table in text, in order.
func tables(text string) [][][]string {
	var out [][][]string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			if t := table(cur); len(t) > 0 {
				out = append(out, t)
			}
			cur = nil
		}
	}
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "|") {
			cur = append(cur, l)
			continue
		}
		flush()
	}
	flush()
	return out
}

// section is the text under the heading named title (any level) up to the next heading of the
// same level or above; "" where there is none.
func section(text, title string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		level, name := heading(l)
		if level == 0 || !strings.EqualFold(name, title) {
			continue
		}
		var out []string
		for _, m := range lines[i+1:] {
			if lv, _ := heading(m); lv != 0 && lv <= level {
				break
			}
			out = append(out, m)
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// heading is a Markdown heading's level and its text without any {% … %} tag; 0 for a line that
// is none.
func heading(l string) (int, string) {
	n := 0
	for n < len(l) && l[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(l) || l[n] != ' ' {
		return 0, ""
	}
	name := strings.TrimSpace(l[n:])
	if i := strings.Index(name, "{%"); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	return n, name
}

// bullets are the items of the lists in text, each without its marker, with Markdown links and
// code marks taken off.
func bullets(text string) []string {
	var out []string
	for l := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			out = append(out, plain(t[2:]))
		}
	}
	return out
}

var (
	reLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reTag  = regexp.MustCompile(`\{%[^%]*%\}`)
	reHTML = regexp.MustCompile(`<[^>]+>`)
)

// plain is s without Markdown's links (their text kept), code and emphasis marks, Markdoc tags
// and HTML.
func plain(s string) string {
	s = reLink.ReplaceAllString(s, "$1")
	s = reTag.ReplaceAllString(s, "")
	s = reHTML.ReplaceAllString(s, " ")
	s = strings.NewReplacer("`", "", "**", "", "__", "", "\\_", "_").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// codeSpans are the `code` spans in s, in order.
func codeSpans(s string) []string {
	var out []string
	for _, m := range reCode.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

var reCode = regexp.MustCompile("`([^`]+)`")

// words are the lowercase words of a list a vendor writes ("text, image", "Text, Image, Video,
// and PDF", "`low`, `medium`"), deduplicated in order.
func words(s string) []string {
	s = strings.NewReplacer("`", "", " and ", ",", " or ", ",", "/", ",").Replace(plain(s))
	var out []string
	for w := range strings.SplitSeq(s, ",") {
		w = strings.ToLower(strings.TrimSpace(w))
		w = strings.TrimSuffix(w, ".")
		if w != "" && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// names says whether text names model by its id or by its name (a display name), as a whole
// name: "Claude Fable 5" is not named by "Claude Fable 5.1".
func names(text, id, name string) bool {
	for _, n := range []string{id, name} {
		if n != "" && wholeIn(text, n) {
			return true
		}
	}
	return false
}

func wholeIn(text, n string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], n)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(n)
		before := start == 0 || !nameChar(text[start-1])
		after := end == len(text) || !nameChar(text[end]) && !(text[end] == '.' && end+1 < len(text) && isDigit(text[end+1]))
		if before && after {
			return true
		}
		i = start + 1
	}
}

func nameChar(c byte) bool {
	return c == '-' || c == '_' || isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// norm is a model id as sources compare it: OpenRouter writes claude-fable-5.2 for Anthropic's
// claude-fable-5-2, Gemini's API models/gemini-4-argon.
func norm(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimPrefix(id, "models/")
	return strings.ReplaceAll(id, ".", "-")
}
