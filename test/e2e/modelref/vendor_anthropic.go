package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Anthropic. Its docs serve every page as Markdown. The deprecations page's status table lists
// every model id the API has served, with its state and dates: that is the list. The overview's
// comparison table, and each model's own page, say the rest.

var anthropic = &vendor{
	id: "anthropic", name: "Anthropic",
	docsURL:      "https://platform.claude.com/docs/en/about-claude/models/overview",
	changelogURL: "https://platform.claude.com/docs/en/release-notes/api",
	changelogMD:  "https://platform.claude.com/docs/en/release-notes/api.md",
	changelogDate: func(h string, level int, _ *dateState) string {
		if level == 3 {
			return isoDate(h)
		}
		return ""
	},
	openrouter: "anthropic", modelsdev: "anthropic",
	keyEnv: "ANTHROPIC_API_KEY", apiLists: true,
	docs:  anthropicDocs,
	api:   anthropicAPI,
	probe: anthropicProbe,
}

const (
	anthropicDocs_      = "https://platform.claude.com/docs/en"
	anthropicDeprecated = anthropicDocs_ + "/about-claude/model-deprecations"
	anthropicOverview   = anthropicDocs_ + "/about-claude/models/overview"
)

func anthropicDocs(ctx context.Context, c *client, now time.Time) ([]observation, error) {
	status, err := c.text(ctx, anthropicDeprecated+".md")
	if err != nil {
		return nil, err
	}
	byID := map[string]*observation{}
	var order []string
	for _, t := range tables(section(status, "Model status")) {
		for _, row := range t[1:] {
			if len(row) < 4 || !strings.HasPrefix(row[0], "claude-") {
				continue
			}
			id := strings.Trim(row[0], "` ")
			f := map[string]any{}
			setFact(f, factStatus, strings.ToLower(row[1]))
			setFact(f, factDeprecatedAt, isoDate(row[2]))
			retire(f, row[3])
			byID[id] = &observation{source: "anthropic-docs", vendor: "anthropic", id: id, url: anthropicDeprecated,
				listed: true, facts: f, docs: map[string]Page{}}
			order = append(order, id)
		}
	}
	if len(byID) == 0 {
		return nil, fmt.Errorf("%s: no model status table", anthropicDeprecated)
	}
	if overview, err := c.text(ctx, anthropicOverview+".md"); err == nil {
		anthropicCompare(overview, byID)
	} else {
		return nil, err
	}
	for _, id := range order {
		o := byID[id]
		page, url, err := anthropicModelPage(ctx, c, id)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		o.url = url
		o.docs["overview"] = pageRead("Model page", url, page)
		anthropicPageFacts(page, o.facts)
		if wn := reWhatsNew.FindStringSubmatch(page); wn != nil {
			if text, err := c.text(ctx, wn[1]+".md"); err == nil {
				o.docs["whats_new"] = pageRead("What's new", wn[1], text)
			} else if !errors.Is(err, errNotFound) {
				return nil, err
			}
		}
	}
	out := make([]observation, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

var reWhatsNew = regexp.MustCompile(`\[What[’']s new\]\((https://platform\.claude\.com/docs/[^)\s]+)\)`)

// retire reads a retirement cell: "Not sooner than November 9, 2027", "August 5, 2026", "To be
// announced".
func retire(f map[string]any, cell string) {
	d := isoDate(cell)
	if d == "" {
		return
	}
	if strings.Contains(strings.ToLower(cell), "not sooner") {
		f[factRetiresNotBefore] = d
	} else {
		f[factRetiresAt] = d
	}
}

// anthropicModelPage is a model's page: /docs/en/models/<slug>/overview, the slug being its id
// without "claude-" and any date, the family first (claude-3-7-sonnet-20250219 is sonnet-3-7).
func anthropicModelPage(ctx context.Context, c *client, id string) (string, string, error) {
	slug := strings.TrimPrefix(id, "claude-")
	slug = reDateSuffix.ReplaceAllString(slug, "")
	candidates := []string{slug}
	if parts := strings.Split(slug, "-"); len(parts) > 1 && isDigit(parts[0][0]) {
		// claude-3-7-sonnet: the family last, as the oldest ids have it.
		candidates = append(candidates, parts[len(parts)-1]+"-"+strings.Join(parts[:len(parts)-1], "-"))
	}
	for _, s := range candidates {
		url := anthropicDocs_ + "/models/" + s + "/overview"
		text, err := c.text(ctx, url+".md")
		if errors.Is(err, errNotFound) {
			continue
		}
		return text, url, err
	}
	return "", "", errNotFound
}

var reDateSuffix = regexp.MustCompile(`-\d{8}$`)

// anthropicCompare reads the overview's comparison table: a column a current model.
func anthropicCompare(text string, byID map[string]*observation) {
	for _, t := range tables(text) {
		if len(t) < 2 || !strings.EqualFold(t[0][0], "Feature") {
			continue
		}
		cols := map[int]*observation{}
		for _, row := range t {
			if !strings.HasPrefix(row[0], "Claude API ID") {
				continue
			}
			for i, cell := range row[1:] {
				if o := byID[strings.Trim(cell, "` ")]; o != nil {
					cols[i+1] = o
				}
			}
		}
		for _, row := range t {
			label := plain(row[0])
			for i, o := range cols {
				if i >= len(row) {
					continue
				}
				anthropicFact(o.facts, label, row[i])
			}
		}
	}
}

// anthropicFact reads one labelled cell, of the overview's table or a model page's.
func anthropicFact(f map[string]any, label, cell string) {
	v := plain(cell)
	switch {
	case label == "Description":
		setFact(f, factDescription, v)
	case label == "Comparative latency" || label == "Latency":
		setFact(f, factLatency, strings.ToLower(v))
	case label == "Thinking":
		setFact(f, factThinking, strings.ToLower(v))
		if v != "" && !strings.EqualFold(v, "No") {
			f[factReasoning] = true
		}
	case label == "Default effort":
		setFact(f, factDefaultEffort, strings.ToLower(strings.Trim(v, "` ")))
	case label == "Context window" || label == "Context":
		if n, ok := tokenCount(v); ok {
			f[factContextWindow] = n
		}
	case label == "Max output":
		if n, ok := tokenCount(v); ok {
			f[factMaxOutput] = n
		}
	case label == "Reliable knowledge cutoff" || label == "Knowledge cutoff":
		setFact(f, factKnowledgeCutoff, isoDate(v))
	case label == "Training data cutoff":
		setFact(f, factTrainingCutoff, isoDate(v))
	case label == "Claude API alias":
		setFact(f, factAliases, codeSpans(cell))
	case strings.HasSuffix(label, " ID") && label != "Claude API ID":
		ids := codeSpans(cell)
		if len(ids) > 0 {
			p, _ := f[factPlatformIDs].(map[string]string)
			if p == nil {
				p = map[string]string{}
			}
			p[strings.TrimSuffix(label, " ID")] = ids[0]
			f[factPlatformIDs] = p
		}
	}
}

var (
	reReleased  = regexp.MustCompile(`Released ([A-Z][a-z]+ \d{1,2}, \d{4})`)
	reHeaderRow = regexp.MustCompile(`(?m)^Context window: ([^·\n]+)·\s*Max output: ([^·\n]+)`)
	reFrontName = regexp.MustCompile(`(?m)\A---\n(?:.*\n)*?title: (.+)\n`)
)

// anthropicPageFacts reads a model's page: its name (the page's title), its release date, its
// headline limits, its row of the comparison table, and its ids on each platform.
func anthropicPageFacts(text string, f map[string]any) {
	if m := reFrontName.FindStringSubmatch(text); m != nil {
		setFact(f, factName, strings.Trim(strings.TrimSpace(m[1]), `"`))
	}
	if m := reReleased.FindStringSubmatch(text); m != nil {
		setFact(f, factReleasedAt, isoDate(m[1]))
	}
	if m := reHeaderRow.FindStringSubmatch(text); m != nil {
		if n, ok := tokenCount(m[1]); ok {
			f[factContextWindow] = n
		}
		if n, ok := tokenCount(m[2]); ok {
			f[factMaxOutput] = n
		}
	}
	for _, t := range tables(text) {
		head := t[0]
		switch {
		case len(head) > 1 && head[0] == "Model" && len(t) > 1:
			for _, row := range t[1:] {
				if !strings.Contains(row[0], "(this model)") {
					continue
				}
				for i := 1; i < len(row) && i < len(head); i++ {
					anthropicFact(f, head[i], row[i])
				}
			}
		case len(head) == 2 && head[0] == "Platform":
			p := map[string]string{}
			for _, row := range t[1:] {
				if ids := codeSpans(row[1]); len(ids) > 0 && plain(row[0]) != "Claude API" {
					p[plain(row[0])] = ids[0]
				}
			}
			setFact(f, factPlatformIDs, p)
		}
	}
}

// anthropicAPI reads GET /v1/models: each model's limits and its capability tree, as the API
// states them.
func anthropicAPI(ctx context.Context, c *client, key string) ([]observation, error) {
	var out []observation
	after := ""
	for range 20 {
		url := "https://api.anthropic.com/v1/models?limit=1000"
		if after != "" {
			url += "&after_id=" + after
		}
		var page struct {
			Data    []map[string]any `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		if err := c.getJSON(ctx, url, anthropicHeaders(key), &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			id, _ := m["id"].(string)
			if id == "" {
				continue
			}
			f := map[string]any{}
			if s, ok := m["display_name"].(string); ok {
				setFact(f, factName, s)
			}
			if n, ok := m["max_input_tokens"].(float64); ok && n > 0 {
				f[factContextWindow] = int64(n)
			}
			if n, ok := m["max_tokens"].(float64); ok && n > 0 {
				f[factMaxOutput] = int64(n)
			}
			if s, ok := m["created_at"].(string); ok {
				setFact(f, factReleasedAt, isoDate(s))
			}
			out = append(out, observation{source: "anthropic-api", vendor: "anthropic", id: id,
				url: "https://api.anthropic.com/v1/models/" + id, listed: true, facts: f, listing: m})
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return out, nil
}

// anthropicVersion is the API version the reference reads and probes with: the newest, as a
// new integration would send it. Probes record it with what they saw.
const anthropicVersion = "2026-10-21"

func anthropicHeaders(key string) map[string]string {
	return map[string]string{"x-api-key": key, "anthropic-version": anthropicVersion}
}
