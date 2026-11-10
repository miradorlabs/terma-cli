package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Announcements: a new model, said where people will see it (the team's Slack; a post for X),
// linking to its notes. A model is news once: the run that first lists it, where its vendor had
// models in the reference before. A vendor published for the first time announces nothing (its
// every model would be news), unless a model's release is on or after -announce-since.

// Announcement is one new model, said ready for each place it is posted.
type Announcement struct {
	Model       string   `json:"model"`
	Vendor      string   `json:"vendor"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Predecessor *string  `json:"predecessor"`
	Facts       []string `json:"facts"`
	Highlights  []string `json:"highlights"`
	// Text is the announcement as a post of at most 280 characters, a link counted as X counts
	// one (23).
	Text  string        `json:"text"`
	Slack *SlackMessage `json:"slack"`
}

// SlackMessage is an announcement as Slack's incoming webhooks take it.
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

// announcements are this run's news.
func announcements(res *result, site, since string) []Announcement {
	out := []Announcement{}
	for _, id := range res.new {
		s := res.snaps[id]
		released, _ := s.Facts[factReleasedAt].(string)
		// A release date known to the month is compared to the month.
		recentRelease := since != "" && released != "" && released >= since[:min(len(since), len(released))]
		if !s.Listed || (!res.vendored[s.Vendor] && !recentRelease) {
			continue
		}
		n := res.notes[id]
		a := Announcement{Model: id, Vendor: s.Vendor, Name: s.Name, URL: fmt.Sprintf("%s/m/%s", site, id),
			Facts: headline(s), Highlights: []string{}}
		if n != nil {
			a.Predecessor = n.Predecessor
			// The vendor's own words first, then what the reference sees differ.
			a.Highlights = append(append([]string{}, n.VendorHighlights...), n.Highlights...)
		}
		a.Text = postText(a, res.snaps)
		a.Slack = slackMessage(a, res.snaps)
		out = append(out, a)
	}
	return out
}

// headline are a model's facts worth a line: its context window, its output limit, its
// knowledge cutoff, whether it reasons.
func headline(s *Snapshot) []string {
	var out []string
	if n, ok := s.Facts[factContextWindow].(int64); ok {
		out = append(out, tokens(n)+" context")
	}
	if n, ok := s.Facts[factMaxOutput].(int64); ok {
		out = append(out, tokens(n)+" output")
	}
	if d, ok := s.Facts[factKnowledgeCutoff].(string); ok {
		out = append(out, "knowledge to "+monthYear(d))
	}
	if e, ok := s.Facts[factReasoningEfforts].([]string); ok && len(e) > 0 {
		out = append(out, "effort "+strings.Join(e, "/"))
	}
	return out
}

func monthYear(d string) string {
	var y, m int
	if _, err := fmt.Sscanf(d, "%d-%d", &y, &m); err != nil || m < 1 || m > 12 {
		return d
	}
	return fmt.Sprintf("%s %d", [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}[m-1], y)
}

func vendorName(id string) string {
	if v := vendorByID(id); v != nil {
		return v.name
	}
	return id
}

// postText is a as one short post: name, headline facts, what is new against the model before,
// and the link.
func postText(a Announcement, snaps map[string]*Snapshot) string {
	const limit, link = 280, 23
	// A post has room for the limits, and the news.
	text := a.Name + " is out"
	if len(a.Facts) > 0 {
		text += ": " + strings.Join(a.Facts[:min(2, len(a.Facts))], ", ")
	}
	text += "."
	room := limit - link - 1
	if len(a.Highlights) > 0 {
		head := text + "\n" + newsHead(a, snaps)
		if utf8.RuneCountInString(head) < room {
			text = head
			for _, h := range a.Highlights {
				next := text + "\n· " + h
				if utf8.RuneCountInString(next) > room {
					break
				}
				text = next
			}
			if strings.HasSuffix(text, ":") {
				text = strings.TrimSuffix(text, "\n"+newsHead(a, snaps))
			}
		}
	}
	return text + "\n" + a.URL
}

func nameOf(id string, snaps map[string]*Snapshot) string {
	if s := snaps[id]; s != nil {
		return s.Name
	}
	return id
}

// slackMessage is a as a Slack message: its headline, what is new against the model before, and
// a button to its notes.
func slackMessage(a Announcement, snaps map[string]*Snapshot) *SlackMessage {
	summary := fmt.Sprintf("*%s* is out (%s)", a.Name, vendorName(a.Vendor))
	if len(a.Facts) > 0 {
		summary += ": " + strings.Join(a.Facts, " · ")
	}
	blocks := []SlackBlock{{Type: "section", Text: &SlackText{Type: "mrkdwn", Text: summary}}}
	if len(a.Highlights) > 0 {
		lines := []string{newsHead(a, snaps)}
		for _, h := range a.Highlights {
			lines = append(lines, "• "+h)
		}
		blocks = append(blocks, SlackBlock{Type: "section", Text: &SlackText{Type: "mrkdwn", Text: strings.Join(lines, "\n")}})
	}
	blocks = append(blocks, SlackBlock{Type: "actions", Elements: []SlackButton{
		{Type: "button", Text: SlackText{Type: "plain_text", Text: "View the notes"}, URL: a.URL}}})
	return &SlackMessage{Blocks: blocks, Text: fmt.Sprintf("%s is out: %s", a.Name, a.URL)}
}

// newsHead introduces a model's highlights: what is new against the model before it, where it
// has one.
func newsHead(a Announcement, snaps map[string]*Snapshot) string {
	if a.Predecessor != nil {
		return "New since " + nameOf(*a.Predecessor, snaps) + ":"
	}
	return "What's new:"
}
