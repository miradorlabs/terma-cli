package main

import (
	"fmt"

	"strings"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// Announcements: a harness's new version, said where people will see it (the team's Slack,
// later the public on X), linking to its page in the reference. A version is news once: the
// night it is first captured, if it is newer than every version of the harness captured
// before. A backfill (older versions), a build whose capture failed (nothing captured yet),
// and a harness published for the first time (every version at once) announce nothing.

// Announcement is one new version, said ready for each place it is posted.
type Announcement struct {
	Harness      string   `json:"harness"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Previous     string   `json:"previous,omitempty"`
	URL          string   `json:"url"`
	CompareURL   string   `json:"compare_url,omitempty"`
	Changes      int      `json:"changes"`
	Impact       ImpactN  `json:"impact"`
	Undocumented *int     `json:"undocumented"`
	Highlights   []string `json:"highlights"`
	Signing      *string  `json:"signing"`
	// Text is the announcement as a post of at most 280 characters, a link counted as X
	// counts one (23).
	Text  string         `json:"text"`
	Slack map[string]any `json:"slack"`
}

// ImpactN counts a version's changes by impact.
type ImpactN map[string]int

// captured reports a snapshot with something captured of the build itself, not only terma's
// verdict on it.
func captured(s *Snapshot) bool {
	for _, sec := range present(s) {
		if sec != "support" {
			return true
		}
	}
	return false
}

// newestCaptured is the newest version with something captured of it, "" for none.
func newestCaptured(byVersion map[string]*Snapshot) string {
	newest := ""
	for v, s := range byVersion {
		if captured(s) && (newest == "" || versionLess(newest, v)) {
			newest = v
		}
	}
	return newest
}

// announce is what a harness has to announce after the night: its versions captured now
// that are newer than the newest captured before.
func announce(site string, entry HarnessEntry, entries []VersionEntry, diffs map[string]*Diff, byVersion map[string]*Snapshot, before string) []Announcement {
	if before == "" || site == "" {
		return nil
	}
	var out []Announcement
	// entries are newest first; announce oldest first, as they came out.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !versionLess(before, e.Version) || !captured(byVersion[e.Version]) {
			continue
		}
		a := Announcement{Harness: entry.ID, Name: entry.Name, Version: e.Version, Changes: e.Changes, Impact: ImpactN(e.Impact),
			Undocumented: e.Undocumented, Highlights: []string{}, Signing: e.Signing,
			URL: fmt.Sprintf("%s/h/%s/%s", site, entry.ID, e.Version)}
		if e.Previous != nil {
			a.Previous = *e.Previous
			a.CompareURL = fmt.Sprintf("%s/harnesses/%s/compare/%s...%s", site, entry.ID, *e.Previous, e.Version)
			if d := diffs[*e.Previous+".."+e.Version]; d != nil {
				a.Highlights = refdiff.Highlights(d, 5)
			}
		}
		a.Text = postText(a)
		a.Slack = slackMessage(a)
		out = append(out, a)
	}
	return out
}

// postText is a as one short post: name and version, what changed, and the link.
func postText(a Announcement) string {
	const limit, link = 280, 23
	head := fmt.Sprintf("%s %s is out.", a.Name, a.Version)
	if a.Previous != "" && a.Changes > 0 {
		head += fmt.Sprintf(" %d %s since %s", a.Changes, plural(a.Changes, "change", "changes"), a.Previous)
		if a.Undocumented != nil && *a.Undocumented > 0 {
			head += fmt.Sprintf(", %d not in the changelog", *a.Undocumented)
		}
		head += ":"
	}
	text := head
	room := limit - link - 1
	for _, h := range a.Highlights {
		next := text + "\n· " + h
		if utf8.RuneCountInString(next) > room {
			break
		}
		text = next
	}
	return text + "\n" + a.URL
}

// slackMessage is a as a Slack message: its headline, what changed, and buttons to its page
// and to the comparison with the version before.
func slackMessage(a Announcement) map[string]any {
	summary := fmt.Sprintf("*%s %s* is out", a.Name, a.Version)
	if a.Previous != "" {
		summary += fmt.Sprintf(": %d %s since %s", a.Changes, plural(a.Changes, "change", "changes"), a.Previous)
	}
	var counts []string
	for _, k := range []string{"breaking", "degrading", "notable"} {
		if n := a.Impact[k]; n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if a.Undocumented != nil && *a.Undocumented > 0 {
		counts = append(counts, fmt.Sprintf("%d not in the vendor's changelog", *a.Undocumented))
	}
	if a.Signing != nil && *a.Signing != "valid" {
		counts = append(counts, "code signature "+*a.Signing)
	}
	if len(counts) > 0 {
		summary += "\n" + strings.Join(counts, " · ")
	}
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": summary}}}
	if len(a.Highlights) > 0 {
		var sb strings.Builder
		for _, h := range a.Highlights {
			sb.WriteString("• " + slackEscape(h) + "\n")
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": strings.TrimSpace(sb.String())}})
	}
	buttons := []any{map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": "View changelog"}, "url": a.URL}}
	if a.CompareURL != "" {
		buttons = append(buttons, map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": "Compare with " + a.Previous}, "url": a.CompareURL})
	}
	blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	return map[string]any{"text": fmt.Sprintf("%s %s is out: %s", a.Name, a.Version, a.URL), "blocks": blocks, "unfurl_links": false}
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
