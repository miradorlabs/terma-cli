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

// Google (the Gemini API). Its docs serve a page as Markdown with .md.txt appended, written in
// Markdoc: a model's facts are a table of {% section %} cells. The models index links each
// model's page; a model is one the reference follows where it reads text and writes text alone
// (not a voice, image, live or embedding model).

var google = &vendor{
	id: "google", name: "Google",
	docsURL:      "https://ai.google.dev/gemini-api/docs/models",
	changelogURL: "https://ai.google.dev/gemini-api/docs/changelog",
	changelogMD:  "https://ai.google.dev/gemini-api/docs/changelog.md.txt",
	changelogDate: func(h string, level int, _ *dateState) string {
		if level == 2 {
			return isoDate(h)
		}
		return ""
	},
	openrouter: "google", modelsdev: "google",
	keyEnv: "GEMINI_API_KEY",
	docs:   googleDocs,
	api:    googleAPI,
	probe:  googleProbe,
}

const googleDocsBase = "https://ai.google.dev/gemini-api/docs"

var (
	reGoogleModelLink = regexp.MustCompile(`(?:href="|\]\()/docs/models/([a-z0-9][a-z0-9.\-]*)`)
	reGoogleWhatsNew  = regexp.MustCompile(`\]\((/docs/whats-new-[a-z0-9.\-]+)\)`)
)

func googleDocs(ctx context.Context, c *client, now time.Time) ([]observation, error) {
	index, err := c.text(ctx, googleDocsBase+"/models.md.txt")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range reGoogleModelLink.FindAllStringSubmatch(index, -1) {
		if !slices.Contains(ids, m[1]) {
			ids = append(ids, m[1])
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no model links", googleDocsBase+"/models")
	}
	var out []observation
	for _, id := range ids {
		url := googleDocsBase + "/models/" + id
		text, err := c.text(ctx, url+".md.txt")
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, o := range googlePage(text, url) {
			if wn := reGoogleWhatsNew.FindStringSubmatch(text); wn != nil {
				wurl := "https://ai.google.dev/gemini-api" + wn[1]
				if t, err := c.text(ctx, wurl+".md.txt"); err == nil {
					o.docs["whats_new"] = pageRead("What's new", wurl, t)
				} else if !errors.Is(err, errNotFound) {
					return nil, err
				}
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// googlePage reads a model's page: one table of properties a model code it lists (a page may
// list a stable model and its previews), each a model the reference follows if it writes text
// alone.
func googlePage(text, url string) []observation {
	var out []observation
	title := ""
	if m := reOpenAITitle.FindStringSubmatch(text); m != nil {
		title = strings.TrimSpace(m[1])
	}
	desc := ""
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p != "" && !strings.HasPrefix(p, "#") && !strings.HasPrefix(p, ">") && !strings.HasPrefix(p, "{%") {
			desc = plain(p)
			break
		}
	}
	for _, block := range strings.Split(text, `{% table variant="model" %}`)[1:] {
		if i := strings.Index(block, "{% /table %}"); i >= 0 {
			block = block[:i]
		}
		props := googleProps(block)
		code := codeSpans(props["model code"])
		if len(code) == 0 {
			continue
		}
		f := map[string]any{}
		setFact(f, factName, title)
		setFact(f, factDescription, desc)
		types := props["supported data types"]
		in, outs := "", ""
		if i := strings.Index(types, "**Inputs**"); i >= 0 {
			in = types[i+len("**Inputs**"):]
			if j := strings.Index(in, "**Output"); j >= 0 {
				outs = in[j:]
				in = in[:j]
			}
		}
		outs = strings.TrimPrefix(strings.TrimPrefix(outs, "**Outputs**"), "**Output**")
		setFact(f, factInputModalities, words(in))
		setFact(f, factOutputModalities, words(outs))
		limits := props["token limits"]
		if m := regexp.MustCompile(`(?s)Context window[^*]*\*\*\s*([\d,]+)`).FindStringSubmatch(limits); m != nil {
			if n, ok := tokenCount(m[1]); ok {
				f[factContextWindow] = n
			}
		} else if m := regexp.MustCompile(`(?s)Input token limit\*\*\s*([\d,]+)`).FindStringSubmatch(limits); m != nil {
			if n, ok := tokenCount(m[1]); ok {
				f[factMaxInput] = n
			}
		}
		if m := regexp.MustCompile(`(?s)(?:Max output|Output token limit)\*\*\s*([\d,]+)`).FindStringSubmatch(limits); m != nil {
			if n, ok := tokenCount(m[1]); ok {
				f[factMaxOutput] = n
			}
		}
		var supported, unsupported []string
		for _, capab := range regexp.MustCompile(`(?s)\[\*\*([^*]+)\*\*\][^{]*\{% badge ([^%]*)/%\}`).FindAllStringSubmatch(props["capabilities"], -1) {
			name := strings.ToLower(strings.TrimSpace(capab[1]))
			if strings.Contains(capab[2], `"not-supported"`) {
				unsupported = append(unsupported, name)
			} else {
				supported = append(supported, name)
			}
		}
		setFact(f, factFeatures, supported)
		setFact(f, factUnsupported, unsupported)
		if len(supported)+len(unsupported) > 0 {
			f[factToolCalling] = slices.Contains(supported, "function calling")
			f[factStructuredOutputs] = slices.Contains(supported, "structured outputs")
			f[factReasoning] = slices.Contains(supported, "thinking")
		}
		setFact(f, factKnowledgeCutoff, isoDate(plain(props["knowledge cutoff"])))
		var versions []string
		for _, s := range codeSpans(props["versions"]) {
			versions = append(versions, s)
		}
		setFact(f, factSnapshots, versions)
		ins, _ := f[factInputModalities].([]string)
		os, _ := f[factOutputModalities].([]string)
		if !slices.Contains(ins, "text") || !slices.Equal(os, []string{"text"}) {
			continue
		}
		out = append(out, observation{source: "google-docs", vendor: "google", id: code[0], url: url, listed: true,
			facts: f, docs: map[string]Page{"overview": pageRead("Model page", url, text)}})
	}
	return out
}

// googleProps are a model table's rows by their property's name, lowercase: each row is a
// "* {% section %}{% icon /%}Name{% /section %}" cell, then the value's cell, up to "---".
func googleProps(block string) map[string]string {
	props := map[string]string{}
	for _, row := range strings.Split(block, "\n  ---") {
		cells := regexp.MustCompile(`(?m)^  \*`).Split(row, -1)
		if len(cells) < 3 {
			continue
		}
		name := strings.ToLower(plain(cells[1]))
		if i := strings.Index(name, "["); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
		props[name] = strings.Join(cells[2:], "\n")
	}
	return props
}

// googleAPI reads GET /v1beta/models: each model's token limits, its generation methods and its
// sampling defaults, as the API states them.
func googleAPI(ctx context.Context, c *client, key string) ([]observation, error) {
	var out []observation
	token := ""
	for range 20 {
		url := "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1000"
		if token != "" {
			url += "&pageToken=" + token
		}
		var page struct {
			Models        []map[string]any `json:"models"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := c.getJSON(ctx, url, map[string]string{"x-goog-api-key": key}, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Models {
			name, _ := m["name"].(string)
			id := strings.TrimPrefix(name, "models/")
			if id == "" {
				continue
			}
			f := map[string]any{}
			if s, ok := m["displayName"].(string); ok {
				setFact(f, factName, s)
			}
			if n, ok := m["inputTokenLimit"].(float64); ok && n > 0 {
				f[factMaxInput] = int64(n)
			}
			if n, ok := m["outputTokenLimit"].(float64); ok && n > 0 {
				f[factMaxOutput] = int64(n)
			}
			if b, ok := m["thinking"].(bool); ok {
				f[factReasoning] = b
			}
			out = append(out, observation{source: "google-api", vendor: "google", id: id,
				url: "https://generativelanguage.googleapis.com/v1beta/" + name, facts: f, listing: m})
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return out, nil
}
