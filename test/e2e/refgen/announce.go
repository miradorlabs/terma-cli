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
	// Support is terma's verdict on the build (refdiff.Support's status), Failed the checks that
	// failed, by label, and Partial whether its capture failed: what the team needs to know of a
	// release first, whether terma still works with it.
	Support string   `json:"support"`
	Failed  []string `json:"failed,omitempty"`
	Partial bool     `json:"partial,omitempty"`
	// Text is the announcement as a post of at most 280 characters, a link counted as X
	// counts one (23).
	Text  string        `json:"text"`
	Slack *SlackMessage `json:"slack"`
}

// SlackMessage is an announcement as Slack's chat.postMessage takes it: its fallback text, its
// blocks, and no link previews. Fields are in the order the JSON lists them.
type SlackMessage struct {
	Blocks      []SlackBlock `json:"blocks"`
	Text        string       `json:"text"`
	UnfurlLinks bool         `json:"unfurl_links"`
}

// SlackBlock is a section of text, or a row of buttons (actions).
type SlackBlock struct {
	Elements []SlackButton `json:"elements,omitempty"`
	Text     *SlackText    `json:"text,omitempty"`
	Type     string        `json:"type"`
}

// SlackButton is a button that opens a link.
type SlackButton struct {
	Text SlackText `json:"text"`
	Type string    `json:"type"`
	URL  string    `json:"url"`
}

// SlackText is a text object: mrkdwn or plain_text.
type SlackText struct {
	Text string `json:"text"`
	Type string `json:"type"`
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
		s := byVersion[e.Version]
		a := Announcement{Harness: entry.ID, Name: entry.Name, Version: e.Version, Changes: e.Changes, Impact: ImpactN(e.Impact),
			Undocumented: e.Undocumented, Highlights: []string{}, Signing: e.Signing,
			URL: fmt.Sprintf("%s/h/%s/%s", site, entry.ID, e.Version), Support: e.Support, Failed: failedChecks(s), Partial: s.Capture.Partial}
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

// failedChecks are the labels of s's capability checks that failed, each once, with the platforms
// it failed on where it did not fail on all.
func failedChecks(s *Snapshot) []string {
	if s.Support == nil {
		return nil
	}
	failed, ran := map[string][]string{}, map[string]int{}
	var order []string
	for _, c := range s.Support.Capabilities {
		label := c.Label
		if label == "" {
			label = c.ID
		}
		ran[label]++
		if c.Result != "fail" {
			continue
		}
		if failed[label] == nil {
			order = append(order, label)
		}
		failed[label] = append(failed[label], c.Platform)
	}
	out := make([]string, 0, len(order))
	for _, label := range order {
		if len(failed[label]) < ran[label] {
			label += " (" + strings.Join(failed[label], ", ") + ")"
		}
		out = append(out, label)
	}
	return out
}

// teamVerdict is what a says of terma and the build, for the team: whether it works, and what broke.
func teamVerdict(a Announcement) string {
	var v string
	switch a.Support {
	case "supported":
		v = ":white_check_mark: *Works with terma*"
	case "degraded":
		v = ":warning: *Partly broken with terma*"
	case "unsupported":
		v = ":x: *Broken with terma*"
	default:
		v = ":grey_question: *Not tested with terma*"
	}
	if len(a.Failed) > 0 {
		v += ": " + slackEscape(strings.Join(a.Failed, "; "))
	}
	if a.Partial {
		v += "\n:hourglass: Its capture failed in part; it is captured again within hours, and its page updates then."
	}
	return v
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
func slackMessage(a Announcement) *SlackMessage {
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
	section := func(text string) SlackBlock {
		return SlackBlock{Type: "section", Text: &SlackText{Type: "mrkdwn", Text: text}}
	}
	button := func(label, url string) SlackButton {
		return SlackButton{Type: "button", Text: SlackText{Type: "plain_text", Text: label}, URL: url}
	}
	blocks := []SlackBlock{section(summary), section(teamVerdict(a))}
	if len(a.Highlights) > 0 {
		var sb strings.Builder
		for _, h := range a.Highlights {
			sb.WriteString("• " + slackEscape(h) + "\n")
		}
		blocks = append(blocks, section(strings.TrimSpace(sb.String())))
	}
	buttons := []SlackButton{button("View changelog", a.URL)}
	if a.CompareURL != "" {
		buttons = append(buttons, button("Compare with "+a.Previous, a.CompareURL))
	}
	blocks = append(blocks, SlackBlock{Type: "actions", Elements: buttons})
	return &SlackMessage{Text: fmt.Sprintf("%s %s is out (%s with terma): %s", a.Name, a.Version, verdictWord(a.Support), a.URL), Blocks: blocks}
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

// verdictWord is terma's verdict in a word, for a notification's plain text.
func verdictWord(support string) string {
	switch support {
	case "supported":
		return "works"
	case "degraded":
		return "partly broken"
	case "unsupported":
		return "broken"
	}
	return "not tested"
}
