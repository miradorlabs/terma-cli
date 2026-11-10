package main

import (
	"context"
	"strings"
	"time"
)

// The two public catalogs: models.dev (an open, community-kept catalog) and OpenRouter (a
// router whose model list says what it serves). Neither decides which models the reference
// follows; each fills facts the vendor's pages leave out, and is named where it says one.

const (
	modelsDevURL  = "https://models.dev/api.json"
	openRouterURL = "https://openrouter.ai/api/v1/models"
)

// modelsDev reads models.dev's catalog for every vendor the reference follows.
func modelsDev(ctx context.Context, c *client) ([]observation, error) {
	var cat map[string]struct {
		Models map[string]struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			Description      string `json:"description"`
			Reasoning        *bool  `json:"reasoning"`
			ReasoningOptions []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			ToolCall         *bool  `json:"tool_call"`
			StructuredOutput *bool  `json:"structured_output"`
			Temperature      *bool  `json:"temperature"`
			Knowledge        string `json:"knowledge"`
			ReleaseDate      string `json:"release_date"`
			OpenWeights      *bool  `json:"open_weights"`
			Modalities       struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Limit struct {
				Context int64 `json:"context"`
				Input   int64 `json:"input"`
				Output  int64 `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := c.getJSON(ctx, modelsDevURL, nil, &cat); err != nil {
		return nil, err
	}
	var out []observation
	for _, v := range vendors {
		p, ok := cat[v.modelsdev]
		if !ok {
			continue
		}
		for key, m := range p.Models {
			id := m.ID
			if id == "" {
				id = key
			}
			f := map[string]any{}
			setFact(f, factName, m.Name)
			setFact(f, factReleasedAt, isoDate(m.ReleaseDate))
			setFact(f, factKnowledgeCutoff, isoDate(m.Knowledge))
			if m.Limit.Context > 0 {
				f[factContextWindow] = m.Limit.Context
			}
			if m.Limit.Input > 0 {
				f[factMaxInput] = m.Limit.Input
			}
			if m.Limit.Output > 0 {
				f[factMaxOutput] = m.Limit.Output
			}
			setFact(f, factInputModalities, lower(m.Modalities.Input))
			setFact(f, factOutputModalities, lower(m.Modalities.Output))
			setBool(f, factReasoning, m.Reasoning)
			setBool(f, factToolCalling, m.ToolCall)
			setBool(f, factStructuredOutputs, m.StructuredOutput)
			setBool(f, factSampling, m.Temperature)
			setBool(f, factOpenWeights, m.OpenWeights)
			for _, o := range m.ReasoningOptions {
				if o.Type == "effort" {
					setFact(f, factReasoningEfforts, lower(o.Values))
				}
			}
			out = append(out, observation{source: "models.dev", vendor: v.id, id: id, url: "https://models.dev", facts: f})
		}
	}
	return out, nil
}

// openRouter reads OpenRouter's model list for every vendor the reference follows. Its
// variants (claude-fable-5.2:batch) are its own, and left out.
func openRouter(ctx context.Context, c *client) ([]observation, error) {
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
			KnowledgeCut  string `json:"knowledge_cutoff"`
			Expiration    string `json:"expiration_date"`
			Architecture  struct {
				Input  []string `json:"input_modalities"`
				Output []string `json:"output_modalities"`
			} `json:"architecture"`
			TopProvider struct {
				ContextLength       int64 `json:"context_length"`
				MaxCompletionTokens int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
			Reasoning           *struct {
				Mandatory        bool     `json:"mandatory"`
				SupportedEfforts []string `json:"supported_efforts"`
				DefaultEffort    string   `json:"default_effort"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, openRouterURL, nil, &list); err != nil {
		return nil, err
	}
	byPrefix := map[string]*vendor{}
	for _, v := range vendors {
		byPrefix[v.openrouter] = v
	}
	var out []observation
	for _, m := range list.Data {
		prefix, slug, ok := strings.Cut(m.ID, "/")
		v := byPrefix[prefix]
		if !ok || v == nil || strings.Contains(slug, ":") {
			continue
		}
		f := map[string]any{}
		name := m.Name
		if _, after, ok := strings.Cut(name, ": "); ok {
			name = after
		}
		setFact(f, factName, name)
		setFact(f, factKnowledgeCutoff, isoDate(m.KnowledgeCut))
		setFact(f, factRetiresAt, isoDate(m.Expiration))
		ctxLen := m.TopProvider.ContextLength
		if ctxLen == 0 {
			ctxLen = m.ContextLength
		}
		if ctxLen > 0 {
			f[factContextWindow] = ctxLen
		}
		if m.TopProvider.MaxCompletionTokens > 0 {
			f[factMaxOutput] = m.TopProvider.MaxCompletionTokens
		}
		setFact(f, factInputModalities, lower(m.Architecture.Input))
		setFact(f, factOutputModalities, lower(m.Architecture.Output))
		if m.Reasoning != nil {
			f[factReasoning] = true
			setFact(f, factReasoningEfforts, lower(m.Reasoning.SupportedEfforts))
			setFact(f, factDefaultEffort, strings.ToLower(m.Reasoning.DefaultEffort))
		}
		hasTools := false
		for _, p := range m.SupportedParameters {
			if p == "tools" {
				hasTools = true
			}
		}
		f[factToolCalling] = hasTools
		out = append(out, observation{source: "openrouter", vendor: v.id, id: slug,
			url: "https://openrouter.ai/" + m.ID, facts: f})
	}
	return out, nil
}

func setBool(f map[string]any, key string, b *bool) {
	if b != nil {
		f[key] = *b
	}
}

func lower(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// catalogs are the public catalogs, read once a run.
var catalogs = []struct {
	name string
	read func(ctx context.Context, c *client) ([]observation, error)
}{
	{"models.dev", modelsDev},
	{"openrouter", openRouter},
}

// pageRead is a page and when it was read, for a source that keeps it.
func pageRead(title, url, text string) Page {
	return Page{Title: title, URL: url, Text: strings.TrimSpace(text) + "\n"}
}

var _ = time.Now
