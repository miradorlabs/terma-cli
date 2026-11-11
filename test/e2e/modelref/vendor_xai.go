package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// xAI. Its docs serve a page as Markdown with .md appended. The models page's text pricing
// table names every text model; each has a page of its own under /developers/models.

var xAI = &vendor{
	id: "xai", name: "xAI",
	docsURL:      "https://docs.x.ai/developers/models",
	changelogURL: "https://docs.x.ai/docs/release-notes",
	changelogMD:  "https://docs.x.ai/docs/release-notes.md",
	// "## November" names a month and no year: the notes run newest first, so a month later in
	// the year than the one before it is the year before.
	changelogDate: func(h string, level int, st *dateState) string {
		if level != 2 {
			return ""
		}
		mo, ok := months[strings.ToLower(strings.TrimSpace(h))]
		if !ok {
			if d := isoDate(h); d != "" {
				fmt.Sscanf(d, "%d", &st.year)
				st.lastMonth = 13
				return d
			}
			return ""
		}
		if st.year == 0 {
			// The newest month is this year's, unless it is later than this month.
			st.year = st.now.Year()
			if mo > st.now.Month() {
				st.year--
			}
		} else if int(mo) >= st.lastMonth {
			st.year--
		}
		st.lastMonth = int(mo)
		return fmt.Sprintf("%04d-%02d", st.year, int(mo))
	},
	openrouter: "x-ai", modelsdev: "xai",
	keyEnv: "XAI_API_KEY", apiLists: true,
	docs:  xaiDocs,
	api:   xaiAPI,
	probe: xaiProbe,
}

const xaiDocsBase = "https://docs.x.ai/developers"

func xaiDocs(ctx context.Context, c *client, now time.Time) ([]observation, error) {
	index, err := c.text(ctx, xaiDocsBase+"/models.md")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range tables(section(index, "Text API Pricing")) {
		for _, row := range t[1:] {
			id := strings.TrimSpace(strings.Split(row[0], "(")[0])
			if id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no text models", xaiDocsBase+"/models")
	}
	var out []observation
	for _, id := range ids {
		url := xaiDocsBase + "/models/" + id
		text, err := c.text(ctx, url+".md")
		f := map[string]any{}
		o := observation{source: "xai-docs", vendor: "xai", id: id, url: url, listed: true, facts: f, docs: map[string]Page{}}
		switch {
		case errors.Is(err, errNotFound):
			// Listed with a price and no page of its own: followed all the same.
		case err != nil:
			return nil, err
		default:
			o.docs["overview"] = pageRead("Model page", url, text)
			xaiPage(text, f)
		}
		// A model's "Get Started" page (grok-4-7), where there is one, says what is new in it.
		wurl := xaiDocsBase + "/" + strings.ReplaceAll(id, ".", "-")
		if t, err := c.text(ctx, wurl+".md"); err == nil {
			o.docs["whats_new"] = pageRead("Get started", wurl, t)
		} else if !errors.Is(err, errNotFound) {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

var reXAIModalities = regexp.MustCompile(`^(.*)→(.*)$`)

// xaiPage reads a model's page: its "At a glance" and "Capabilities" lists, and its regions.
func xaiPage(text string, f map[string]any) {
	if m := reOpenAITitle.FindStringSubmatch(text); m != nil {
		setFact(f, factName, strings.TrimSpace(m[1]))
	}
	for _, p := range strings.Split(text, "\n\n")[1:] {
		if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "#") {
			setFact(f, factDescription, plain(p))
			break
		}
	}
	for _, b := range append(bullets(section(text, "At a glance")), bullets(section(text, "Capabilities"))...) {
		label, value, ok := strings.Cut(b, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		yes := strings.EqualFold(value, "yes") || strings.EqualFold(value, "supported")
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "modalities":
			if m := reXAIModalities.FindStringSubmatch(value); m != nil {
				setFact(f, factInputModalities, words(m[1]))
				setFact(f, factOutputModalities, words(m[2]))
			}
		case "context window":
			if n, ok := tokenCount(value); ok {
				f[factContextWindow] = n
			}
		case "max output", "max output tokens":
			if n, ok := tokenCount(value); ok {
				f[factMaxOutput] = n
			}
		case "batch api":
			f[factBatch] = yes
		case "function calling":
			f[factToolCalling] = yes
		case "structured outputs":
			f[factStructuredOutputs] = yes
		case "reasoning":
			f[factReasoning] = yes
		case "reasoning efforts (supported)":
			setFact(f, factReasoningEfforts, words(value))
		case "reasoning efforts (default)":
			setFact(f, factDefaultEffort, strings.ToLower(strings.Trim(value, "` ")))
		case "knowledge cutoff":
			setFact(f, factKnowledgeCutoff, isoDate(value))
		}
	}
	if r := section(text, "Regions"); r != "" {
		if _, list, ok := strings.Cut(r, "Available in:"); ok {
			setFact(f, factRegions, words(strings.Split(list, "\n")[0]))
		}
	}
}

// xaiAPI reads GET /v1/language-models: each model's aliases, modalities, fingerprint and when
// it was created.
func xaiAPI(ctx context.Context, c *client, key string) ([]observation, error) {
	var list struct {
		Models []map[string]any `json:"models"`
	}
	if err := c.getJSON(ctx, "https://api.x.ai/v1/language-models", map[string]string{"Authorization": "Bearer " + key}, &list); err != nil {
		return nil, err
	}
	var out []observation
	for _, m := range list.Models {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		f := map[string]any{}
		if n, ok := m["created"].(float64); ok && n > 0 {
			f[factReleasedAt] = time.Unix(int64(n), 0).UTC().Format(time.DateOnly)
		}
		if a, ok := m["aliases"].([]any); ok {
			var aliases []string
			for _, x := range a {
				if s, ok := x.(string); ok {
					aliases = append(aliases, s)
				}
			}
			setFact(f, factAliases, aliases)
		}
		listing := map[string]any{}
		for k, v := range m {
			// Prices are not what the reference follows (yet), and move without the model doing so.
			if !strings.Contains(k, "price") {
				listing[k] = v
			}
		}
		out = append(out, observation{source: "xai-api", vendor: "xai", id: id,
			url: "https://api.x.ai/v1/language-models/" + id, listed: true, facts: f, listing: listing})
	}
	return out, nil
}
