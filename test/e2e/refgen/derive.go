package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// Deriving a snapshot from a normalized profile. A harness's model request says most of it:
// its model and parameters, system prompt and injected context, tools, cache breakpoints,
// headers and betas. Four wire formats cover the harnesses: Anthropic Messages, OpenAI
// Responses, OpenAI Chat Completions and Gemini's generateContent. The behaviour probes'
// requests say how the build retries, compacts, truncates and delegates; the out-of-band
// captures fill the rest as they recorded it.

// body is a request's JSON body as a map, nil where it has none.
func body(r e2e.ProviderRequest) map[string]any {
	var m map[string]any
	if len(r.Body) == 0 || json.Unmarshal(r.Body, &m) != nil {
		return nil
	}
	return m
}

// wire is the format of a model request, "" for a request that is not one.
func wire(b map[string]any) string {
	switch {
	case b == nil:
		return ""
	case b["contents"] != nil:
		return "gemini"
	case b["input"] != nil && b["model"] != nil:
		return "responses"
	case b["messages"] != nil && b["model"] != nil:
		if _, ok := b["system"]; ok || b["max_tokens"] != nil && b["anthropic_version"] == nil && hasAnthropicBlocks(b) {
			return "messages"
		}
		return "chat"
	}
	return ""
}

// hasAnthropicBlocks reports messages whose content is Anthropic's typed blocks.
func hasAnthropicBlocks(b map[string]any) bool {
	msgs, _ := b["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if blocks, ok := mm["content"].([]any); ok {
			for _, blk := range blocks {
				if t, _ := blk.(map[string]any)["type"].(string); t == "tool_result" || t == "tool_use" {
					return true
				}
			}
		}
	}
	return false
}

// modelRequests are probe's requests that are model calls, in arrival order.
func modelRequests(p e2e.Profile, probe string) []e2e.ProviderRequest {
	var out []e2e.ProviderRequest
	for _, r := range p.Requests {
		if r.Probe == probe && wire(body(r)) != "" {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// mainRequest is the first turn's agent request: of the first turn's model calls, the one
// offering the most tools, then the largest. An auxiliary call (a title, a quota check) offers
// fewer.
func mainRequest(p e2e.Profile) (e2e.ProviderRequest, bool) {
	reqs := modelRequests(p, "first-turn")
	if len(reqs) == 0 {
		return e2e.ProviderRequest{}, false
	}
	best := reqs[0]
	for _, r := range reqs[1:] {
		bt, rt := len(toolsOf(body(best))), len(toolsOf(body(r)))
		if rt > bt || rt == bt && len(r.Body) > len(best.Body) {
			best = r
		}
	}
	return best, true
}

// deriveRequestSections fills the sections a model request says.
func deriveRequestSections(s *Snapshot, p e2e.Profile) {
	main, ok := mainRequest(p)
	if !ok {
		return
	}
	b := body(main)
	w := wire(b)
	s.Model = deriveModel(b, w)
	s.Prompt = derivePrompt(b, w)
	tools := toolsOf(b)
	s.Tools = make([]any, 0, len(tools))
	offered := map[string]bool{}
	for _, t := range tools {
		s.Tools = append(s.Tools, t)
		offered[t["name"].(string)] = true
	}
	// Tools a build names but defers (their schemas loaded on demand, through its tool search):
	// known to the model by name alone.
	for _, name := range deferredTools(b) {
		if !offered[name] {
			s.Tools = append(s.Tools, map[string]any{"name": name, "deferred": true})
		}
	}
	sort.SliceStable(s.Tools, func(i, j int) bool {
		return s.Tools[i].(map[string]any)["name"].(string) < s.Tools[j].(map[string]any)["name"].(string)
	})
	s.Caching = deriveCaching(b, w)
	s.Request = deriveRequest(main, b, w)
	s.SideRequests = deriveSideRequests(p, main)
}

// modelParamsSkip are the body keys a model section does not list as parameters: what the
// other sections say, and what every request carries.
var modelParamsSkip = map[string]bool{"model": true, "messages": true, "system": true, "tools": true, "input": true,
	"instructions": true, "contents": true, "systemInstruction": true, "system_instruction": true, "stream": true,
	"metadata": true, "max_tokens": true, "max_output_tokens": true, "max_completion_tokens": true,
	"prompt_cache_key": true, "client_metadata": true, "store": true, "stream_options": true, "user": true}

func deriveModel(b map[string]any, w string) map[string]any {
	m := map[string]any{"default": b["model"]}
	for _, k := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		if v, ok := b[k]; ok {
			m["max_tokens"] = v
		}
	}
	if w == "gemini" {
		if gc, ok := b["generationConfig"].(map[string]any); ok {
			if v, ok := gc["maxOutputTokens"]; ok {
				m["max_tokens"] = v
			}
		}
	}
	params := map[string]any{}
	for k, v := range b {
		if !modelParamsSkip[k] {
			params[k] = v
		}
	}
	m["params"] = params
	return m
}

// textBlock is one piece of prompt text, with the cache marker it carried.
func textBlock(text string, cache any) map[string]any {
	blk := map[string]any{"text": text, "chars": len(text), "sha256": sum(text)}
	if cache != nil {
		blk["cache"] = cacheOf(cache)
	}
	return blk
}

// cacheOf is a cache_control marker with its TTL said, Anthropic's default where it is not.
func cacheOf(cc any) map[string]any {
	m, _ := cc.(map[string]any)
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	if _, ok := out["ttl"]; !ok {
		out["ttl"] = "5m"
	}
	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// texts are the text pieces of a content value: a string, or blocks with text.
func texts(content any) []map[string]any {
	switch c := content.(type) {
	case string:
		return []map[string]any{textBlock(c, nil)}
	case []any:
		var out []map[string]any
		for _, blk := range c {
			m, _ := blk.(map[string]any)
			if t, ok := m["text"].(string); ok {
				out = append(out, textBlock(t, m["cache_control"]))
			}
		}
		return out
	}
	return nil
}

// isPrompt reports the scenario's own prompt, which is no part of what the build adds.
func isPrompt(text string) bool { return strings.TrimSpace(text) == "<prompt>" }

func derivePrompt(b map[string]any, w string) map[string]any {
	var system, context []map[string]any
	addContext := func(role string, content any) {
		for _, t := range texts(content) {
			if !isPrompt(t["text"].(string)) {
				t["role"] = role
				context = append(context, t)
			}
		}
	}
	switch w {
	case "messages":
		system = texts(b["system"])
		if msgs, _ := b["messages"].([]any); len(msgs) > 0 {
			first, _ := msgs[0].(map[string]any)
			if first["role"] == "user" {
				addContext("user", first["content"])
			}
			// A system message among the turns (Claude Code's environment, its deferred tools and
			// agent types) is the build's too.
			for _, msg := range msgs {
				if m, _ := msg.(map[string]any); m["role"] == "system" {
					addContext("system", m["content"])
				}
			}
		}
	case "responses":
		if s, ok := b["instructions"].(string); ok && s != "" {
			system = append(system, textBlock(s, nil))
		}
		items, _ := b["input"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["type"] != nil && m["type"] != "message" {
				continue
			}
			switch m["role"] {
			case "developer", "system":
				system = append(system, texts(m["content"])...)
			case "user":
				addContext("user", m["content"])
			}
		}
	case "chat":
		msgs, _ := b["messages"].([]any)
		for _, msg := range msgs {
			m, _ := msg.(map[string]any)
			switch m["role"] {
			case "system", "developer":
				system = append(system, texts(m["content"])...)
			case "user":
				addContext("user", m["content"])
			}
		}
	case "gemini":
		si, _ := b["systemInstruction"].(map[string]any)
		if si == nil {
			si, _ = b["system_instruction"].(map[string]any)
		}
		system = texts(si["parts"])
		contents, _ := b["contents"].([]any)
		for _, c := range contents {
			m, _ := c.(map[string]any)
			if m["role"] == "user" {
				addContext("user", m["parts"])
			}
		}
	}
	return promptOf(system, context)
}

// promptOf is a prompt section from its system and context pieces.
func promptOf(system, context []map[string]any) map[string]any {
	all := strings.Builder{}
	sys := make([]any, 0, len(system))
	for _, t := range system {
		sys = append(sys, t)
		all.WriteString(t["text"].(string))
	}
	ctx := make([]any, 0, len(context))
	for _, t := range context {
		ctx = append(ctx, t)
		all.WriteString(t["text"].(string))
	}
	return map[string]any{"system": sys, "context": ctx, "chars": all.Len(), "sha256": sum(all.String())}
}

// toolsOf are the tools a model request offers, each {name, description, input_schema,
// sha256}, sorted by name. Codex nests its tools in namespaces, in an input item of their own.
func toolsOf(b map[string]any) []map[string]any {
	var raw []any
	if t, ok := b["tools"].([]any); ok {
		raw = append(raw, t...)
	}
	if items, ok := b["input"].([]any); ok {
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["type"] == "additional_tools" {
				t, _ := m["tools"].([]any)
				raw = append(raw, t...)
			}
		}
	}
	var out []map[string]any
	var add func(t any, namespace string)
	add = func(t any, namespace string) {
		m, _ := t.(map[string]any)
		if m == nil {
			return
		}
		if decls, ok := m["functionDeclarations"].([]any); ok {
			for _, d := range decls {
				add(d, namespace)
			}
			return
		}
		if fn, ok := m["function"].(map[string]any); ok && m["type"] == "function" {
			m = fn
		}
		if children, ok := m["tools"].([]any); ok && m["type"] == "namespace" {
			ns, _ := m["name"].(string)
			for _, c := range children {
				add(c, ns)
			}
			return
		}
		name, _ := m["name"].(string)
		if name == "" {
			name, _ = m["type"].(string)
		}
		tool := map[string]any{"name": name}
		if namespace != "" && namespace != "functions" {
			tool["namespace"] = namespace
		}
		if d, ok := m["description"].(string); ok {
			tool["description"] = d
		}
		for _, k := range []string{"input_schema", "parameters", "parametersJsonSchema", "format"} {
			if v, ok := m[k]; ok {
				tool["input_schema"] = v
				break
			}
		}
		if t, ok := m["type"].(string); ok && t != "function" {
			tool["type"] = t
		}
		data, _ := json.Marshal(m)
		tool["sha256"] = sum(string(data))
		out = append(out, tool)
	}
	for _, t := range raw {
		add(t, "")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

// breakpoints are where a request marks the cache: each {at, type, ttl}.
func breakpoints(b map[string]any) []any {
	out := []any{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if cc, ok := x["cache_control"]; ok {
				bp := cacheOf(cc)
				bp["at"] = path
				out = append(out, bp)
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if k != "cache_control" {
					walk(strings.TrimPrefix(path+"."+k, "."), x[k])
				}
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("", b)
	return out
}

func deriveCaching(b map[string]any, w string) map[string]any {
	c := map[string]any{"breakpoints": breakpoints(b)}
	_, key := b["prompt_cache_key"]
	c["prompt_cache_key"] = key
	if v, ok := b["prompt_cache_retention"]; ok {
		c["retention"] = v
	}
	if w == "gemini" {
		if v, ok := b["cachedContent"]; ok {
			c["cached_content"] = v != nil
		}
	}
	return c
}

// volatileHeaders say nothing of a build: the transport's, or what changes request to request.
var volatileHeaders = map[string]bool{"content-length": true, "connection": true, "host": true, "accept-encoding": true,
	"x-stainless-retry-count": true, "x-stainless-timeout": true, "content-type": true, "accept": true,
	"x-stainless-arch": true, "x-stainless-os": true, "x-stainless-runtime-version": true,
	"traceparent": true, "tracestate": true, "baggage": true}

// identifierHint names the headers and body keys that identify an install, a user or a session.
var identifierHint = regexp.MustCompile(`(?i)(session|install|user_id|device|account|thread|window|conversation|request-id|machine|turn-metadata)`)

func deriveRequest(r e2e.ProviderRequest, b map[string]any, w string) map[string]any {
	headers := map[string]any{}
	var betas []string
	var identifiers []string
	for name, values := range r.Header {
		if volatileHeaders[name] || len(values) == 0 {
			continue
		}
		v := strings.Join(values, ", ")
		switch {
		case name == "anthropic-beta" || name == "x-codex-beta-features" || name == "openai-beta":
			for b := range strings.SplitSeq(v, ",") {
				if b = strings.TrimSpace(b); b != "" {
					betas = append(betas, b)
				}
			}
			continue
		case identifierHint.MatchString(name) || v == "[redacted]":
			identifiers = append(identifiers, "header:"+name)
			if v != "[redacted]" {
				continue
			}
		}
		headers[name] = v
	}
	keys := make([]string, 0, len(b))
	for k, v := range b {
		keys = append(keys, k)
		if sub, ok := v.(map[string]any); ok && (k == "metadata" || k == "client_metadata") {
			for sk := range sub {
				if identifierHint.MatchString(sk) {
					identifiers = append(identifiers, k+"."+sk)
				}
			}
		}
	}
	sort.Strings(keys)
	sort.Strings(betas)
	sort.Strings(identifiers)
	req := map[string]any{"endpoint": r.Method + " " + r.Path, "wire": w, "headers": headers, "betas": nonNil(betas),
		"body_keys": keys, "identifiers": nonNil(identifiers)}
	if r.Query != "" {
		req["query"] = r.Query
	}
	if s, ok := b["stream"].(bool); ok {
		req["stream"] = s
	}
	return req
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// deriveSideRequests are the first turn's requests besides its agent request: connectivity
// checks, analytics, auxiliary model calls (a title, a quota check), each once.
func deriveSideRequests(p e2e.Profile, main e2e.ProviderRequest) []any {
	seen := map[string]bool{}
	out := []any{}
	mainAt := main.At
	for _, r := range p.Requests {
		if r.Probe != "first-turn" || r.At.Equal(main.At) && r.Path == main.Path && len(r.Body) == len(main.Body) {
			continue
		}
		b := body(r)
		entry := map[string]any{"method": r.Method, "path": r.Path}
		key := r.Method + " " + r.Path
		if w := wire(b); w != "" {
			entry["model"] = b["model"]
			entry["tools"] = len(toolsOf(b))
			key += fmt.Sprint(" ", b["model"], " ", len(toolsOf(b)))
			if p := derivePrompt(b, w); len(p["system"].([]any)) > 0 {
				entry["system"] = p["system"]
			}
		} else if b != nil {
			keys := make([]string, 0, len(b))
			for k := range b {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			entry["body_keys"] = keys
		}
		switch {
		case r.At.Before(mainAt):
			entry["when"] = "before the first model request"
		default:
			entry["when"] = "after the first model request"
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, entry)
		}
	}
	return out
}

// deriveBehavior reads the behaviour probes: their requests where the requests say it, their
// recorded findings where they do not.
func deriveBehavior(s *Snapshot, p e2e.Profile) {
	beh := map[string]any{}
	probes := map[string]bool{}
	for _, r := range p.Requests {
		if r.Probe != "" && r.Probe != "first-turn" {
			probes[r.Probe] = true
		}
	}
	mainSystem := ""
	if s.Prompt != nil {
		mainSystem, _ = s.Prompt["sha256"].(string)
	}
	for _, probe := range slices.Sorted(func(yield func(string) bool) {
		for k := range probes {
			if !yield(k) {
				return
			}
		}
	}) {
		reqs := probeRequests(p, probe)
		switch {
		case strings.HasPrefix(probe, "retries-"):
			retries, _ := beh["retries"].(map[string]any)
			if retries == nil {
				retries = map[string]any{"statuses": map[string]any{}}
				beh["retries"] = retries
			}
			retries["statuses"].(map[string]any)[strings.TrimPrefix(probe, "retries-")] = retriesOf(reqs)
			if fb := fallbackModel(reqs); fb != "" {
				retries["fallback_model"] = fb
			}
		case strings.HasPrefix(probe, "model-alias-"):
			if m := firstModel(reqs); m != "" {
				if s.Model == nil {
					s.Model = map[string]any{}
				}
				aliases, _ := s.Model["aliases"].(map[string]any)
				if aliases == nil {
					aliases = map[string]any{}
					s.Model["aliases"] = aliases
				}
				aliases[strings.TrimPrefix(probe, "model-alias-")] = m
			}
		case probe == "interactive":
			if main := largest(reqs); main != nil {
				b := body(*main)
				pr := derivePrompt(b, wire(b))
				if s.Prompt == nil {
					s.Prompt = map[string]any{}
				}
				s.Prompt["interactive"] = map[string]any{"system": pr["system"], "context": pr["context"], "chars": pr["chars"], "sha256": pr["sha256"],
					"model": b["model"], "tools": names(toolsOf(b))}
			}
		case probe == "live-flags":
			if main := largest(reqs); main != nil {
				b := body(*main)
				pr := derivePrompt(b, wire(b))
				s.Live = map[string]any{"prompt": pr, "model": b["model"], "tools": names(toolsOf(b)),
					"differs": s.Prompt != nil && pr["sha256"] != s.Prompt["sha256"]}
			}
		case probe == "compaction" || probe == "compaction-command":
			if c := compactionOf(reqs); c != nil {
				beh[behaviorKey(probe)] = c
			}
		case probe == "compaction-setup":
			// The turns that fill the context before a compaction: no finding of their own.
		case probe == "subagent":
			if sa := subagentOf(reqs, mainSystem); sa != nil {
				beh["subagents"] = sa
			}
		case probe == "tool-output":
			if to := toolOutputOf(reqs); to != nil {
				beh["tool_output"] = to
			}
		case probe == "across-turns":
			if s.Caching == nil {
				s.Caching = map[string]any{}
			}
			s.Caching["across_turns"] = acrossTurns(reqs)
		default:
			beh[behaviorKey(probe)] = map[string]any{"requests": len(reqs), "models": modelsOf(reqs)}
		}
	}
	for name, raw := range p.Probes {
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		switch name {
		case "prices":
			if s.Model == nil {
				s.Model = map[string]any{}
			}
			s.Model["prices"] = v
		case "hooks":
			if m, ok := v.(map[string]any); ok {
				s.Hooks = m
			}
		case "hooks-first-turn":
			// The payloads of the hooks the plain turn ran ({events: …}): the hooks section where
			// the build had no hooks probe.
			if _, probed := p.Probes["hooks"]; !probed {
				if m, ok := v.(map[string]any); ok {
					s.Hooks = m
				}
			}
		case "telemetry", "signing", "across-turns":
			// Merged with the census in deriveTelemetry; the binary's in deriveOutOfBand; the
			// caching's from the requests.
		default:
			if code, ok := strings.CutPrefix(name, "retries-"); ok {
				// What the probe saw of the run's end (whether it succeeded, its error) joins
				// what its requests say of the retries.
				retries, _ := beh["retries"].(map[string]any)
				if retries == nil {
					retries = map[string]any{"statuses": map[string]any{}}
					beh["retries"] = retries
				}
				statuses := retries["statuses"].(map[string]any)
				st, _ := statuses[code].(map[string]any)
				if st == nil {
					st = map[string]any{}
					statuses[code] = st
				}
				if m, ok := v.(map[string]any); ok {
					// A run cut short by the probe's own time limit (a busy machine) says nothing
					// of how the build retries: not observed.
					if e, _ := m["error"].(string); strings.Contains(e, "did not finish") {
						delete(statuses, code)
						continue
					}
					for k, x := range m {
						if k != "status" {
							st[k] = x
						}
					}
				}
				continue
			}
			name = behaviorKey(name)
			if existing, ok := beh[name].(map[string]any); ok {
				if m, ok := v.(map[string]any); ok {
					for k, x := range m {
						existing[k] = x
					}
					continue
				}
			}
			beh[name] = v
		}
	}
	if len(beh) > 0 {
		s.Behavior = beh
	}
}

// behaviorKey is where a probe's findings sit in the behaviour section: its name, with
// underscores.
func behaviorKey(probe string) string {
	switch probe {
	case "subagent":
		return "subagents"
	}
	return strings.ReplaceAll(probe, "-", "_")
}

// probeRequests are probe's requests, model calls or not, in arrival order.
func probeRequests(p e2e.Profile, probe string) []e2e.ProviderRequest {
	var out []e2e.ProviderRequest
	for _, r := range p.Requests {
		if r.Probe == probe {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// retriesOf is what a retry probe's requests say: how many attempts the build made, the
// waits between them, and whether it waited out the fixture's retry-after of 2 s.
func retriesOf(reqs []e2e.ProviderRequest) map[string]any {
	var model []e2e.ProviderRequest
	for _, r := range reqs {
		if wire(body(r)) != "" {
			model = append(model, r)
		}
	}
	backoff := []int64{}
	honors := len(model) > 1
	for i := 1; i < len(model); i++ {
		gap := model[i].At.Sub(model[i-1].At).Milliseconds()
		backoff = append(backoff, gap)
		if model[i-1].Status >= 400 && gap < 1900 {
			honors = false
		}
	}
	failed := 0
	for _, r := range model {
		if r.Status >= 400 {
			failed++
		}
	}
	return map[string]any{"attempts": len(model), "failed": failed, "backoff_ms": backoff, "honors_retry_after": honors,
		"recovered": len(model) > 0 && model[len(model)-1].Status < 400}
}

// fallbackModel is a model the build turned to after a failure, "" where it kept its own.
func fallbackModel(reqs []e2e.ProviderRequest) string {
	first := firstModel(reqs)
	for _, r := range reqs {
		if m, _ := body(r)["model"].(string); m != "" && m != first {
			return m
		}
	}
	return ""
}

func firstModel(reqs []e2e.ProviderRequest) string {
	for _, r := range reqs {
		if m, _ := body(r)["model"].(string); m != "" {
			return m
		}
	}
	return ""
}

func modelsOf(reqs []e2e.ProviderRequest) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range reqs {
		if m, _ := body(r)["model"].(string); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func largest(reqs []e2e.ProviderRequest) *e2e.ProviderRequest {
	var best *e2e.ProviderRequest
	for i := range reqs {
		if wire(body(reqs[i])) == "" {
			continue
		}
		if best == nil || len(reqs[i].Body) > len(best.Body) {
			best = &reqs[i]
		}
	}
	return best
}

func names(tools []map[string]any) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t["name"].(string))
	}
	return out
}

// lastUserText is the text of a model request's last user turn.
func lastUserText(b map[string]any) string {
	var items []any
	switch wire(b) {
	case "messages", "chat":
		items, _ = b["messages"].([]any)
	case "responses":
		items, _ = b["input"].([]any)
	case "gemini":
		items, _ = b["contents"].([]any)
	}
	for i := len(items) - 1; i >= 0; i-- {
		m, _ := items[i].(map[string]any)
		if m["role"] != "user" {
			continue
		}
		content := m["content"]
		if content == nil {
			content = m["parts"]
		}
		var sb strings.Builder
		for _, t := range texts(content) {
			sb.WriteString(t["text"].(string))
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

var summaryHint = regexp.MustCompile(`(?i)summar|compact`)

// compactionOf is the compaction request of a compaction probe: the one whose own prompt
// asks for a summary, its prompt and model, and the input tokens the fixture had reported
// before it.
func compactionOf(reqs []e2e.ProviderRequest) map[string]any {
	// The first that asks for one: the turns after it carry the summary in their context.
	for i := range reqs {
		b := body(reqs[i])
		if wire(b) == "" {
			continue
		}
		text := lastUserText(b)
		pr := derivePrompt(b, wire(b))
		sys := ""
		for _, t := range pr["system"].([]any) {
			sys += t.(map[string]any)["text"].(string)
		}
		if summaryHint.MatchString(text) || summaryHint.MatchString(sys) && len(toolsOf(b)) == 0 {
			// Its own instruction, not the system prompt around it: that is the agent's, and says
			// again each change of the prompt section.
			return map[string]any{"model": b["model"], "prompt": text, "tools": len(toolsOf(b))}
		}
	}
	return nil
}

// subagentOf is a subagent's request in a subagent probe: a model call whose system prompt
// is not the agent's own.
func subagentOf(reqs []e2e.ProviderRequest, mainSHA string) map[string]any {
	// A request that says it is a subagent's: Claude Code's billing header, Codex's turn
	// metadata naming an agent other than the root.
	for _, r := range reqs {
		b := body(r)
		w := wire(b)
		if w == "" || !isSubagent(r, b) {
			continue
		}
		pr := derivePrompt(b, w)
		return map[string]any{"model": b["model"], "system": pr["system"], "tools": names(toolsOf(b))}
	}
	for _, r := range reqs {
		b := body(r)
		w := wire(b)
		if w == "" {
			continue
		}
		pr := derivePrompt(b, w)
		if pr["sha256"] == mainSHA || len(pr["system"].([]any)) == 0 {
			continue
		}
		if len(toolsOf(b)) == 0 {
			continue // an auxiliary call, not an agent
		}
		return map[string]any{"model": b["model"], "system": pr["system"], "tools": names(toolsOf(b))}
	}
	return nil
}

var (
	truncationMarker = regexp.MustCompile(`(?i)[^\n]{0,40}(truncat|omitted|elided|characters? (?:were )?removed|too large|persisted-output)[^\n]{0,40}`)
)

// isSubagent reports a request a subagent sent: Claude Code marks it in its billing header,
// Codex names the agent in its turn metadata, "/root" for the agent the user runs.
func isSubagent(r e2e.ProviderRequest, b map[string]any) bool {
	if strings.Contains(string(r.Body), "cc_is_subagent=true") {
		return true
	}
	meta, _ := b["client_metadata"].(map[string]any)
	raw, _ := meta["x-codex-turn-metadata"].(string)
	var turn struct {
		Agent string `json:"agent_name"`
	}
	return json.Unmarshal([]byte(raw), &turn) == nil && turn.Agent != "" && turn.Agent != "/root"
}

// toolOutputOf is what a tool output probe's build sent back of the tool's ~400 KB: the
// length of the longest tool result and the marker it put where it cut.
func toolOutputOf(reqs []e2e.ProviderRequest) map[string]any {
	longest := ""
	for _, r := range reqs {
		for _, t := range toolResults(body(r)) {
			if len(t) > len(longest) {
				longest = t
			}
		}
	}
	if longest == "" {
		return nil
	}
	out := map[string]any{"limit_chars": len(longest)}
	if m := truncationMarker.FindString(longest); m != "" {
		out["marker"] = strings.TrimSpace(m)
	}
	// The probe's tool numbers its lines (TERMA-LINE-000001…): the numbers that reached the
	// model say which part the build kept.
	var first, last, kept int
	for _, m := range numberedLine.FindAllStringSubmatch(longest, -1) {
		n, _ := strconv.Atoi(m[1])
		if kept == 0 || n < first {
			first = n
		}
		last = max(last, n)
		kept++
	}
	if kept > 0 {
		out["kept_lines"] = map[string]int{"count": kept, "first": first, "last": last}
		switch head, tail := first == 1, last >= probeLines; {
		case head && tail:
			out["truncation"] = "middle"
		case head:
			out["truncation"] = "end"
		case tail:
			out["truncation"] = "start"
		}
	}
	return out
}

// numberedLine is a line of the tool output probe's command; probeLines how many it prints.
var numberedLine = regexp.MustCompile(`TERMA-LINE-0*(\d+)`)

const probeLines = 25000

// toolResults are the texts of the tool results a model request sends back.
func toolResults(b map[string]any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			t, _ := x["type"].(string)
			if t == "tool_result" || t == "function_call_output" || t == "custom_tool_call_output" || x["role"] == "tool" || x["functionResponse"] != nil {
				data := ""
				for _, k := range []string{"content", "output", "functionResponse"} {
					switch c := x[k].(type) {
					case string:
						data += c
					case nil:
					default:
						for _, tb := range texts(c) {
							data += tb["text"].(string)
						}
						if data == "" {
							raw, _ := json.Marshal(c)
							data = string(raw)
						}
					}
				}
				out = append(out, data)
				return
			}
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(b)
	return out
}

// acrossTurns is how a session's requests turn to turn keep the cache: the turns seen, where
// each puts its breakpoints, whether the cache key stays, and the prefix they share.
func acrossTurns(reqs []e2e.ProviderRequest) map[string]any {
	var model []map[string]any
	for _, r := range reqs {
		if b := body(r); wire(b) != "" && len(toolsOf(b)) > 0 {
			model = append(model, b)
		}
	}
	out := map[string]any{"turns": len(model)}
	if len(model) == 0 {
		return out
	}
	per := []any{}
	keys := map[string]bool{}
	for _, b := range model {
		var at []string
		for _, bp := range breakpoints(b) {
			at = append(at, bp.(map[string]any)["at"].(string))
		}
		per = append(per, nonNil(at))
		if k, ok := b["prompt_cache_key"].(string); ok {
			keys[k] = true
		}
	}
	out["breakpoints"] = per
	out["stable_cache_key"] = len(keys) == 1
	stable := -1
	for i := 1; i < len(model); i++ {
		a, _ := json.Marshal(model[i-1])
		b, _ := json.Marshal(model[i])
		n := commonPrefix(string(a), string(b))
		if stable < 0 || n < stable {
			stable = n
		}
	}
	if stable >= 0 {
		out["stable_prefix_chars"] = stable
	}
	return out
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deriveOutOfBand fills what the out-of-band captures recorded: egress, files, the binary
// and what it reads.
func deriveOutOfBand(s *Snapshot, p e2e.Profile) {
	if len(p.Egress) > 0 {
		seen := map[string]bool{}
		egress := []any{}
		for _, r := range p.Egress {
			key := r.Method + " " + r.Host + " " + r.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			e := map[string]any{"host": r.Host, "method": r.Method, "path": r.Path}
			if b := body(r); b != nil {
				e["body_keys"] = slices.Sorted(func(yield func(string) bool) {
					for k := range b {
						if !yield(k) {
							return
						}
					}
				})
			}
			if r.When != "" {
				e["when"] = r.When
			}
			if r.Error != "" {
				// A host whose TLS the build refused to the recording proxy: pinned.
				e["error"] = r.Error
			}
			egress = append(egress, e)
		}
		sort.SliceStable(egress, func(i, j int) bool {
			a, b := egress[i].(map[string]any), egress[j].(map[string]any)
			return fmt.Sprint(a["host"], a["path"], a["method"]) < fmt.Sprint(b["host"], b["path"], b["method"])
		})
		s.Network = map[string]any{"egress": egress}
	}
	if len(p.Files) > 0 {
		s.Storage = map[string]any{}
		for kind, raw := range p.Files {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				s.Storage[kind] = v
			}
		}
	}
	if len(p.Binary) > 0 {
		var bin map[string]any
		if json.Unmarshal(p.Binary, &bin) == nil {
			cfg := map[string]any{}
			for _, k := range []string{"env_vars", "env_vars_uncertain", "env_source", "settings_keys", "settings"} {
				if v, ok := bin[k]; ok {
					cfg[k] = v
					delete(bin, k)
				}
			}
			configFromBinary(s.Harness, cfg)
			if len(cfg) > 0 {
				s.Config = cfg
			}
			delete(bin, "sha256") // a build's bytes differ from platform to platform; size says enough
			s.Binary = bin
		}
	}
	// Who signed the build, with which certificate, and whether Gatekeeper still accepts it.
	if raw, ok := p.Probes["signing"]; ok {
		var sig map[string]any
		if json.Unmarshal(raw, &sig) == nil {
			if s.Binary == nil {
				s.Binary = map[string]any{}
			}
			s.Binary["signing"] = sig
		}
	}
}

// deferredList is where Claude Code lists the tools it defers: after it says their schemas
// are not loaded, one name a line, to the first blank line.
var deferredList = regexp.MustCompile(`(?s)(?:schemas are NOT loaded|deferred tools?)[^\n]*:\n((?:[A-Za-z_][\w.:-]*\n)+)`)

// deferredTools are the tools a model request names without offering them.
func deferredTools(b map[string]any) []string {
	var raw string
	// The list is in a message's text: read it unescaped.
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.Contains(x, "NOT loaded") || strings.Contains(x, "deferred tool") {
				raw += x + "\n"
			}
		case map[string]any:
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(b)
	var out []string
	for _, m := range deferredList.FindAllStringSubmatch(raw, -1) {
		for name := range strings.SplitSeq(strings.TrimSpace(m[1]), "\n") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
