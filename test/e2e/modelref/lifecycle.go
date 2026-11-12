package main

import (
	"regexp"
	"strings"
	"time"
)

// Lifecycles: when a vendor says a model goes away, and what to use instead. OpenAI's
// deprecations page lists each deprecation under the day it was announced, with a table of
// shutdown dates, models and replacements; Google's lists every Gemini model in Markdoc tables,
// current, preview and deprecated, each with its release date, its shutdown date and its
// replacement. Neither decides which models the reference follows: they say more of the ones it
// does.

const factReplacement = "replacement"

// openAIDeprecations reads OpenAI's deprecations page.
func openAIDeprecations(text, url string, now time.Time) []observation {
	var out []observation
	announced := ""
	for l := range strings.SplitSeq(text, "\n") {
		if level, h := heading(l); level == 3 {
			// "### 2026-11-10: GPT Image 2"
			announced = isoDate(strings.SplitN(h, ":", 2)[0])
		}
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			continue
		}
		rows := table([]string{l})
		if len(rows) != 1 || len(rows[0]) < 3 {
			continue
		}
		row := rows[0]
		shutdown := isoDate(row[0])
		if shutdown == "" {
			continue // the header row
		}
		for _, id := range codeSpans(row[1]) {
			f := map[string]any{factRetiresAt: shutdown, factStatus: lifecycle(shutdown, now)}
			setFact(f, factDeprecatedAt, announced)
			setFact(f, factReplacement, plain(row[2]))
			out = append(out, observation{source: "openai-docs", vendor: "openai", id: id, url: url, facts: f})
		}
	}
	return out
}

// lifecycle is a deprecated model's status: retired once its shutdown date has passed.
func lifecycle(shutdown string, now time.Time) string {
	if shutdown <= now.Format(time.DateOnly) {
		return "retired"
	}
	return "deprecated"
}

var reMarkdocTable = regexp.MustCompile(`(?s)\{% table %\}(.*?)\{% /table %\}`)

// googleDeprecations reads Google's deprecations page: under a "Preview models" heading a
// model is in preview, under "Deprecated models" deprecated (retired past its shutdown date),
// under a family's own heading current.
func googleDeprecations(text, url string, now time.Time) []observation {
	var out []observation
	status := "active"
	pos := 0
	for _, loc := range reMarkdocTable.FindAllStringSubmatchIndex(text, -1) {
		for l := range strings.SplitSeq(text[pos:loc[0]], "\n") {
			if level, h := heading(l); level == 2 {
				status = "active"
			} else if level == 3 {
				switch lh := strings.ToLower(h); {
				case strings.Contains(lh, "preview"):
					status = "preview"
				case strings.Contains(lh, "deprecated"):
					status = "deprecated"
				}
			}
		}
		pos = loc[1]
		rows := markdocRows(text[loc[2]:loc[3]])
		if len(rows) < 2 {
			continue
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			col[strings.ToLower(plain(h))] = i
		}
		mi, ok := col["model"]
		if !ok {
			continue
		}
		by := func(row []string, name string) string {
			if i, ok := col[name]; ok {
				return cell(row, i)
			}
			return ""
		}
		for _, row := range rows[1:] {
			ids := codeSpans(cell(row, mi))
			if len(ids) == 0 {
				continue
			}
			f := map[string]any{}
			setFact(f, factReleasedAt, isoDate(by(row, "release date")))
			shutdown := isoDate(by(row, "shutdown date"))
			st := status
			if shutdown != "" {
				f[factRetiresAt] = shutdown
				if st == "deprecated" {
					st = lifecycle(shutdown, now)
				}
			}
			f[factStatus] = st
			setFact(f, factReplacement, plain(by(row, "recommended replacement")))
			out = append(out, observation{source: "google-docs", vendor: "google", id: ids[0], url: url, facts: f})
		}
	}
	return out
}

// markdocRows are a Markdoc table's rows ("---" between them), each its "* " cells.
func markdocRows(body string) [][]string {
	var rows [][]string
	for _, part := range strings.Split(body, "\n  ---") {
		var cells []string
		for _, c := range regexp.MustCompile(`(?m)^  \* ?`).Split(part, -1)[1:] {
			cells = append(cells, strings.TrimSpace(c))
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	return rows
}

func cell(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}
