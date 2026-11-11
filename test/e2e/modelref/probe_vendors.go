package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Each vendor's probe cases. The plain request asks for one short word, so an accepted case
// costs a few input tokens and at most maxProbeOutput output tokens; a refused one costs
// nothing. The cases are the parameters a coding agent sets (sampling, thinking and effort,
// forced tool use, structured output, the output limit) and the ones a vendor has changed
// before.

const (
	probePrompt    = "Reply with the word ok."
	maxProbeOutput = 16
)

// probeTool is a function tool for the tool-choice cases.
var probeTool = map[string]any{
	"name":        "get_time",
	"description": "Returns the current time in a time zone.",
}

var probeSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"word": map[string]any{"type": "string"}},
	"required":             []any{"word"},
	"additionalProperties": false,
}

// overLimit is one token over the model's stated output limit, 0 where none is stated.
func overLimit(m *Snapshot) int64 {
	if n, ok := m.Facts[factMaxOutput].(int64); ok {
		return n + 1
	}
	return 0
}

func withOverLimit(cases []probeCase, key string, m *Snapshot, body func(n int64) map[string]any) []probeCase {
	if n := overLimit(m); n > 0 {
		cases = append(cases, probeCase{name: key + ".over_stated_limit", body: body(n)})
	}
	return cases
}

func anthropicProbe(ctx context.Context, c *client, key string, m *Snapshot) *Probes {
	tool := map[string]any{"name": probeTool["name"], "description": probeTool["description"],
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]any{"type": "string"}}}}
	cases := []probeCase{
		{name: "baseline"},
		{name: "sampling.temperature", body: map[string]any{"temperature": 0.5}},
		{name: "sampling.top_p", body: map[string]any{"top_p": 0.9}},
		{name: "sampling.top_k", body: map[string]any{"top_k": 10}},
		{name: "stop_sequences", body: map[string]any{"stop_sequences": []any{"END"}}},
		{name: "thinking.adaptive", body: map[string]any{"thinking": map[string]any{"type": "adaptive"}}},
		{name: "thinking.adaptive.display_summarized", body: map[string]any{"thinking": map[string]any{"type": "adaptive", "display": "summarized"}}},
		{name: "thinking.disabled", body: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{name: "thinking.between_tools", body: map[string]any{"thinking": map[string]any{"type": "between_tools"}}},
		{name: "thinking.budget_tokens", body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}, "max_tokens": 1040}},
		{name: "tool_choice.auto", body: map[string]any{"tools": []any{tool}, "tool_choice": map[string]any{"type": "auto"}}},
		{name: "tool_choice.any", body: map[string]any{"tools": []any{tool}, "tool_choice": map[string]any{"type": "any"}}},
		{name: "tool_choice.tool", body: map[string]any{"tools": []any{tool}, "tool_choice": map[string]any{"type": "tool", "name": "get_time"}}},
		{name: "tool_choice.none", body: map[string]any{"tools": []any{tool}, "tool_choice": map[string]any{"type": "none"}}},
		{name: "prefill", body: map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": probePrompt},
			map[string]any{"role": "assistant", "content": "o"}}}},
		{name: "structured_output", body: map[string]any{"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": probeSchema}}}},
		{name: "speed.fast", body: map[string]any{"speed": "fast"}},
		{name: "inference_geo.us", body: map[string]any{"inference_geo": "us"}},
	}
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		cases = append(cases, probeCase{name: "effort." + e, body: map[string]any{"output_config": map[string]any{"effort": e}}})
	}
	cases = withOverLimit(cases, "max_tokens", m, func(n int64) map[string]any { return map[string]any{"max_tokens": n, "stream": true} })
	h := anthropicHeaders(key)
	base := map[string]any{"model": m.ID, "max_tokens": maxProbeOutput,
		"messages": []any{map[string]any{"role": "user", "content": probePrompt}}}
	t := probeTarget{
		url: "https://api.anthropic.com/v1/messages", headers: h, base: base, cases: cases,
		shape: shapeOf("model", "usage"),
		stream: func(base map[string]any) (string, map[string]any) {
			return "https://api.anthropic.com/v1/messages", merge(base, map[string]any{"stream": true})
		},
		count: func(ctx context.Context, c *client, text string) (int, error) {
			resp, err := c.post(ctx, "https://api.anthropic.com/v1/messages/count_tokens", h,
				map[string]any{"model": m.ID, "messages": []any{map[string]any{"role": "user", "content": text}}})
			return countFrom(resp, err, "input_tokens")
		},
	}
	return runProbes(ctx, c, t, "POST /v1/messages (anthropic-version "+anthropicVersion+")", time.Now().UTC())
}

func openAIProbe(ctx context.Context, c *client, key string, m *Snapshot) *Probes {
	tool := map[string]any{"type": "function", "name": probeTool["name"], "description": probeTool["description"],
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]any{"type": "string"}}}}
	cases := []probeCase{
		{name: "baseline"},
		{name: "sampling.temperature", body: map[string]any{"temperature": 0.5}},
		{name: "sampling.top_p", body: map[string]any{"top_p": 0.9}},
		{name: "reasoning.summary.auto", body: map[string]any{"reasoning": map[string]any{"summary": "auto"}}},
		{name: "text.verbosity.low", body: map[string]any{"text": map[string]any{"verbosity": "low"}}},
		{name: "tool_choice.auto", body: map[string]any{"tools": []any{tool}, "tool_choice": "auto"}},
		{name: "tool_choice.required", body: map[string]any{"tools": []any{tool}, "tool_choice": "required"}},
		{name: "tool_choice.function", body: map[string]any{"tools": []any{tool}, "tool_choice": map[string]any{"type": "function", "name": "get_time"}}},
		{name: "structured_output", body: map[string]any{"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "word", "strict": true, "schema": probeSchema}}}},
		{name: "service_tier.flex", body: map[string]any{"service_tier": "flex"}},
		{name: "service_tier.priority", body: map[string]any{"service_tier": "priority"}},
		{name: "chat_completions", path: "https://api.openai.com/v1/chat/completions", body: map[string]any{
			"input": del, "max_output_tokens": del, "max_completion_tokens": maxProbeOutput,
			"messages": []any{map[string]any{"role": "user", "content": probePrompt}}}},
	}
	for _, e := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		cases = append(cases, probeCase{name: "reasoning.effort." + e, body: map[string]any{"reasoning": map[string]any{"effort": e}}})
	}
	cases = withOverLimit(cases, "max_output_tokens", m, func(n int64) map[string]any { return map[string]any{"max_output_tokens": n, "stream": true} })
	h := map[string]string{"Authorization": "Bearer " + key}
	base := map[string]any{"model": m.ID, "input": probePrompt, "max_output_tokens": maxProbeOutput, "store": false}
	t := probeTarget{
		url: "https://api.openai.com/v1/responses", headers: h, base: base, cases: cases,
		shape: shapeOf("model", "usage"),
		stream: func(base map[string]any) (string, map[string]any) {
			return "https://api.openai.com/v1/responses", merge(base, map[string]any{"stream": true})
		},
		count: func(ctx context.Context, c *client, text string) (int, error) {
			resp, err := c.post(ctx, "https://api.openai.com/v1/responses/input_tokens", h, map[string]any{"model": m.ID, "input": text})
			return countFrom(resp, err, "input_tokens")
		},
	}
	return runProbes(ctx, c, t, "POST /v1/responses", time.Now().UTC())
}

func googleProbe(ctx context.Context, c *client, key string, m *Snapshot) *Probes {
	base := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": probePrompt}}}},
		"generationConfig": map[string]any{"maxOutputTokens": maxProbeOutput},
	}
	gen := func(cfg map[string]any) map[string]any { return map[string]any{"generationConfig": cfg} }
	tools := []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": probeTool["name"], "description": probeTool["description"],
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]any{"type": "string"}}}}}}}
	cases := []probeCase{
		{name: "baseline"},
		{name: "sampling.temperature", body: gen(map[string]any{"temperature": 0.5})},
		{name: "sampling.top_p", body: gen(map[string]any{"topP": 0.9})},
		{name: "sampling.top_k", body: gen(map[string]any{"topK": 10})},
		{name: "stop_sequences", body: gen(map[string]any{"stopSequences": []any{"END"}})},
		{name: "thinking.budget_0", body: gen(map[string]any{"thinkingConfig": map[string]any{"thinkingBudget": 0}})},
		{name: "thinking.budget_1024", body: gen(map[string]any{"thinkingConfig": map[string]any{"thinkingBudget": 1024}})},
		{name: "thinking.include_thoughts", body: gen(map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true}})},
		{name: "structured_output", body: gen(map[string]any{"responseMimeType": "application/json", "responseJsonSchema": probeSchema})},
		{name: "tool_choice.auto", body: map[string]any{"tools": tools, "toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}}},
		{name: "tool_choice.any", body: map[string]any{"tools": tools, "toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}}},
	}
	for _, l := range []string{"minimal", "low", "medium", "high"} {
		cases = append(cases, probeCase{name: "thinking.level." + l, body: gen(map[string]any{"thinkingConfig": map[string]any{"thinkingLevel": l}})})
	}
	cases = withOverLimit(cases, "max_output_tokens", m, func(n int64) map[string]any { return gen(map[string]any{"maxOutputTokens": n}) })
	h := map[string]string{"x-goog-api-key": key}
	api := "https://generativelanguage.googleapis.com/v1beta/models/" + m.ID
	t := probeTarget{
		url: api + ":generateContent", headers: h, base: base, cases: cases,
		shape: shapeOf("modelVersion", "usageMetadata"),
		count: func(ctx context.Context, c *client, text string) (int, error) {
			resp, err := c.post(ctx, api+":countTokens", h,
				map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}})
			return countFrom(resp, err, "totalTokens")
		},
	}
	return runProbes(ctx, c, t, "POST /v1beta/models/{model}:generateContent", time.Now().UTC())
}

func xaiProbe(ctx context.Context, c *client, key string, m *Snapshot) *Probes {
	tool := map[string]any{"type": "function", "function": map[string]any{"name": probeTool["name"], "description": probeTool["description"],
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]any{"type": "string"}}}}}
	cases := []probeCase{
		{name: "baseline"},
		{name: "sampling.temperature", body: map[string]any{"temperature": 0.5}},
		{name: "sampling.top_p", body: map[string]any{"top_p": 0.9}},
		{name: "penalty.presence", body: map[string]any{"presence_penalty": 0.5}},
		{name: "stop_sequences", body: map[string]any{"stop": []any{"END"}}},
		{name: "tool_choice.auto", body: map[string]any{"tools": []any{tool}, "tool_choice": "auto"}},
		{name: "tool_choice.required", body: map[string]any{"tools": []any{tool}, "tool_choice": "required"}},
		{name: "structured_output", body: map[string]any{"response_format": map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "word", "strict": true, "schema": probeSchema}}}},
	}
	for _, e := range []string{"none", "low", "medium", "high", "xhigh"} {
		cases = append(cases, probeCase{name: "reasoning_effort." + e, body: map[string]any{"reasoning_effort": e}})
	}
	cases = withOverLimit(cases, "max_tokens", m, func(n int64) map[string]any { return map[string]any{"max_tokens": n, "stream": true} })
	h := map[string]string{"Authorization": "Bearer " + key}
	base := map[string]any{"model": m.ID, "max_tokens": maxProbeOutput,
		"messages": []any{map[string]any{"role": "user", "content": probePrompt}}}
	t := probeTarget{
		url: "https://api.x.ai/v1/chat/completions", headers: h, base: base, cases: cases,
		shape: shapeOf("model", "usage"),
		stream: func(base map[string]any) (string, map[string]any) {
			return "https://api.x.ai/v1/chat/completions", merge(base, map[string]any{"stream": true})
		},
		count: func(ctx context.Context, c *client, text string) (int, error) {
			resp, err := c.post(ctx, "https://api.x.ai/v1/tokenize-text", h, map[string]any{"model": m.ID, "text": text})
			if err != nil {
				return 0, err
			}
			if resp.Status/100 != 2 {
				return 0, fmt.Errorf("%d %s", resp.Status, apiError(resp.Body))
			}
			var r struct {
				TokenIDs []any `json:"token_ids"`
			}
			if err := json.Unmarshal(resp.Body, &r); err != nil {
				return 0, err
			}
			return len(r.TokenIDs), nil
		},
	}
	return runProbes(ctx, c, t, "POST /v1/chat/completions", time.Now().UTC())
}

// shapeOf reads a plain answer's shape from its body: the model it names under modelKey, and the
// keys of the usage object under usageKey.
func shapeOf(modelKey, usageKey string) func(body []byte) ResponseShape {
	return func(body []byte) ResponseShape {
		var r map[string]any
		_ = json.Unmarshal(body, &r)
		model, _ := r[modelKey].(string)
		return ResponseShape{Model: model, UsageKeys: usageKeys(r[usageKey])}
	}
}

// countFrom is the count a token-counting endpoint answered with, in its field key.
func countFrom(resp *apiResponse, err error, key string) (int, error) {
	if err != nil {
		return 0, err
	}
	if resp.Status/100 != 2 {
		return 0, fmt.Errorf("%d %s", resp.Status, apiError(resp.Body))
	}
	var v map[string]any
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		return 0, err
	}
	n, ok := v[key].(float64)
	if !ok {
		return 0, fmt.Errorf("no %s in the answer", key)
	}
	return int(n), nil
}
