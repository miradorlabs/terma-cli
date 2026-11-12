package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// OpenAI. Its docs serve a page as Markdown with .md appended. The models index links each
// model's page; a model is one the reference follows where its page says it reads text and
// writes text through the Responses or Chat Completions API (not an embedding, image, voice or
// moderation model).

var openAI = &vendor{
	id: "openai", name: "OpenAI",
	docsURL:      "https://developers.openai.com/api/docs/models",
	changelogURL: "https://developers.openai.com/api/docs/changelog",
	changelogMD:  "https://developers.openai.com/api/docs/changelog.md",
	changelogDate: func(h string, level int, st *dateState) string {
		// "## November, 2026" sets the year; "### Nov 5" is a day of it.
		switch level {
		case 2:
			if d := isoDate(strings.ReplaceAll(h, ",", "")); d != "" {
				fmt.Sscanf(d, "%d", &st.year)
			}
		case 3:
			if st.year > 0 {
				return isoDate(fmt.Sprintf("%s, %d", h, st.year))
			}
		}
		return ""
	},
	openrouter: "openai", modelsdev: "openai",
	keyEnv: "OPENAI_API_KEY",
	docs:   openAIDocs,
	api:    openAIAPI,
	probe:  openAIProbe,
}

const openAIDocsBase = "https://developers.openai.com"

var reOpenAIModelLink = regexp.MustCompile(`\]\((/api/docs/models/([a-z0-9][a-z0-9.\-]*))\.md\)`)

func openAIDocs(ctx context.Context, c *client, now time.Time) ([]observation, error) {
	index, err := c.text(ctx, openAIDocsBase+"/api/docs/models.md")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, m := range reOpenAIModelLink.FindAllStringSubmatch(index, -1) {
		if m[2] != "all" && !slices.Contains(paths, m[1]) {
			paths = append(paths, m[1])
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s: no model links", openAIDocsBase+"/api/docs/models")
	}
	var out []observation
	for _, p := range paths {
		url := openAIDocsBase + p
		text, err := c.text(ctx, url+".md")
		if errors.Is(err, errNotFound) {
			// Linked and not there (yet, or for now): listed still, as it was.
			out = append(out, observation{source: "openai-docs", vendor: "openai", id: path.Base(p), url: url, listed: true, stale: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		o, ok := openAIPage(text, url)
		if ok {
			out = append(out, o)
		}
	}
	// What is going away, when, and what to use instead.
	dep := openAIDocsBase + "/api/docs/deprecations"
	text, err := c.text(ctx, dep+".md")
	if errors.Is(err, errNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return append(out, openAIDeprecations(text, dep, now)...), nil
}

var (
	reOpenAIModelID = regexp.MustCompile("(?m)^Model ID: `([^`]+)`")
	// The levels run to the sentence's end, which may wrap: no level has a full stop in it.
	reOpenAIEffort  = regexp.MustCompile("`reasoning\\.effort` supports ([^.]+)\\.")
	reOpenAITitle   = regexp.MustCompile(`(?m)^# (.+)$`)
	reOpenAITagline = regexp.MustCompile(`(?m)^> ([^\n]+)$`)
)

// openAIPage reads a model's page; ok false for a model the reference does not follow.
func openAIPage(text, url string) (observation, bool) {
	f := map[string]any{}
	id := ""
	if m := reOpenAIModelID.FindStringSubmatch(text); m != nil {
		id = m[1]
	}
	if m := reOpenAITitle.FindStringSubmatch(text); m != nil {
		setFact(f, factName, strings.TrimSpace(m[1]))
	}
	for _, m := range reOpenAITagline.FindAllStringSubmatch(text, -1) {
		if !strings.Contains(m[1], "llms.txt") {
			setFact(f, factDescription, plain(m[1]))
			break
		}
	}
	for _, b := range bullets(section(text, "Model details")) {
		lb := strings.ToLower(b)
		switch {
		case strings.HasPrefix(lb, "default snapshot:"):
			if id == "" {
				id = strings.TrimSpace(b[len("default snapshot:"):])
			}
		case strings.HasPrefix(lb, "input modalities:"):
			setFact(f, factInputModalities, words(b[len("input modalities:"):]))
		case strings.HasPrefix(lb, "output modalities:"):
			setFact(f, factOutputModalities, words(b[len("output modalities:"):]))
		case strings.HasPrefix(lb, "maximum input tokens:"):
			if n, ok := tokenCount(b[len("maximum input tokens:"):]); ok {
				f[factMaxInput] = n
			}
		case strings.HasSuffix(lb, "context window"):
			if n, ok := tokenCount(b); ok {
				f[factContextWindow] = n
			}
		case strings.HasSuffix(lb, "max output tokens"):
			if n, ok := tokenCount(b); ok {
				f[factMaxOutput] = n
			}
		case strings.HasSuffix(lb, "knowledge cutoff"):
			setFact(f, factKnowledgeCutoff, isoDate(b))
		case strings.Contains(lb, "reasoning token support"):
			f[factReasoning] = true
		}
	}
	if m := reOpenAIEffort.FindStringSubmatch(text); m != nil {
		efforts := []string{}
		for _, part := range strings.Split(m[1], ",") {
			for _, c := range codeSpans(part) {
				efforts = append(efforts, strings.ToLower(c))
				if strings.Contains(part, "(default)") {
					f[factDefaultEffort] = strings.ToLower(c)
				}
			}
		}
		setFact(f, factReasoningEfforts, efforts)
		f[factReasoning] = true
	}
	setFact(f, factFeatures, lower(bullets(section(text, "Supported features"))))
	setFact(f, factUnsupported, lower(bullets(section(text, "Unsupported features"))))
	setFact(f, factTools, lower(bullets(section(text, "Supported tools"))))
	var snaps []string
	for _, b := range bullets(section(text, "Snapshots")) {
		snaps = append(snaps, strings.TrimSpace(b))
	}
	setFact(f, factSnapshots, snaps)
	var endpoints []string
	textOut := false
	for _, t := range tables(section(text, "Endpoints")) {
		for _, row := range t[1:] {
			if len(row) >= 3 && strings.EqualFold(row[2], "Supported") {
				endpoints = append(endpoints, strings.Trim(row[1], "` "))
				if row[0] == "Responses" || row[0] == "Chat Completions" {
					textOut = true
				}
			}
		}
	}
	setFact(f, factEndpoints, endpoints)
	platforms := map[string]string{}
	for _, t := range tables(section(text, "Cloud providers")) {
		for _, row := range t[1:] {
			if len(row) >= 2 {
				if ids := codeSpans(row[1]); len(ids) > 0 {
					platforms[strings.TrimSpace(strings.Split(plain(row[0]), " Private")[0])] = ids[0]
				}
			}
		}
	}
	setFact(f, factPlatformIDs, platforms)
	if f[factFeatures] != nil {
		feats := f[factFeatures].([]string)
		f[factToolCalling] = slices.Contains(feats, "function_calling")
		f[factStructuredOutputs] = slices.Contains(feats, "structured_outputs")
	}
	outs, _ := f[factOutputModalities].([]string)
	ins, _ := f[factInputModalities].([]string)
	if id == "" || !textOut || !slices.Contains(outs, "text") || !slices.Contains(ins, "text") || slices.Contains(outs, "audio") {
		return observation{}, false
	}
	return observation{source: "openai-docs", vendor: "openai", id: id, url: url, listed: true, facts: f,
		docs: map[string]Page{"overview": pageRead("Model page", url, text)}}, true
}

// openAIAPI reads GET /v1/models: the ids the key may use and when each was created.
func openAIAPI(ctx context.Context, c *client, key string) ([]observation, error) {
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, "https://api.openai.com/v1/models", map[string]string{"Authorization": "Bearer " + key}, &list); err != nil {
		return nil, err
	}
	var out []observation
	for _, m := range list.Data {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		f := map[string]any{}
		if n, ok := m["created"].(float64); ok && n > 0 {
			f[factReleasedAt] = time.Unix(int64(n), 0).UTC().Format(time.DateOnly)
		}
		// The API lists every model the key may use, embeddings and images too: only a model
		// its docs list is followed (listed false), and this fills what the docs leave out.
		out = append(out, observation{source: "openai-api", vendor: "openai", id: id,
			url: "https://api.openai.com/v1/models/" + id, facts: f, listing: m})
	}
	return out, nil
}
