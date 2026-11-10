package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// The behaviour section: what the behaviour probes found of a build (retries, fallback,
// compaction, subagents, tool output limits, caching across turns), read from their requests
// where the requests say it, from their recorded findings where they do not.

// deriveBehavior reads the behaviour probes: their requests where the requests say it, their
// recorded findings where they do not.
func deriveBehavior(s *Snapshot, p e2e.Profile) {
	beh := map[string]any{}
	probes := map[string]bool{}
	for _, r := range p.Requests {
		if r.Probe != "" && r.Probe != "first-turn" && r.Probe != "filesystem" {
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
				s.Hooks = pidsPlaced(m)
			}
		case "hooks-first-turn":
			// The payloads of the hooks the plain turn ran ({events: …}): the hooks section where
			// the build had no hooks probe.
			if _, probed := p.Probes["hooks"]; !probed {
				if m, ok := v.(map[string]any); ok {
					s.Hooks = m
				}
			}
		case "telemetry", "telemetry-routing", "signing", "across-turns", "filesystem", "launch", "app-server":
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
