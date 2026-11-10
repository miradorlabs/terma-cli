package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
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
		if t := vendorTelemetry(p.Egress); t != nil {
			s.Network["telemetry"] = t
		}
	}
	// Where the build's own telemetry can be sent, and what goes with it (Muse's telemetry
	// routing probe): beside what it sends.
	if raw, ok := p.Probes["telemetry-routing"]; ok {
		var v map[string]any
		if json.Unmarshal(raw, &v) == nil && v["routes"] != nil {
			if s.Network == nil {
				s.Network = map[string]any{}
			}
			s.Network["telemetry_routing"] = v["routes"]
		}
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
			configFromBinary(agentOf(s.Harness), cfg)
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

// pidsPlaced is a hooks section with the process ids its events' variables held in
// placeholders (CLAUDE_PID=<pid>): a capture taken before the probe placed them publishes none.
func pidsPlaced(hooks map[string]any) map[string]any {
	events, _ := hooks["events"].(map[string]any)
	for _, ev := range events {
		vals, _ := ev.(map[string]any)["env_values"].(map[string]any)
		for k, v := range vals {
			if str, ok := v.(string); ok && e2e.RunPID(k, str) {
				vals[k] = "<pid>"
			}
		}
	}
	return hooks
}
