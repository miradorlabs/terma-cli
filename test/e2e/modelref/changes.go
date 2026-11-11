package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// What changed: a model against itself the run before (its timeline), and a model against the
// one before it in its family (its notes). Both compare a view of the snapshot, as JSON: what
// the model is and does, not when or where it was read.

// view is what a timeline compares of a snapshot.
func view(s *Snapshot) map[string]any {
	v := map[string]any{"facts": s.Facts, "listed": s.Listed}
	docs := map[string]any{}
	for name, p := range s.Docs {
		docs[name] = p.Text
	}
	v["docs"] = docs
	if s.API != nil {
		api := map[string]any{}
		if s.API.Listing != nil {
			api["listing"] = s.API.Listing
		}
		if p := s.API.Probes; p != nil {
			api["probes"] = probeView(p)
		}
		v["api"] = api
	}
	h := map[string]any{}
	for _, u := range s.Harnesses {
		h[useKey(u)] = map[string]any{"current": u.Current}
	}
	v["harnesses"] = h
	return roundTrip(v)
}

func probeView(p *Probes) map[string]any {
	pv := map[string]any{"params": p.Params, "endpoint": p.Endpoint}
	if p.Unavailable != "" {
		pv["unavailable"] = p.Unavailable
	}
	if p.Tokens != nil {
		pv["tokens"] = p.Tokens
	}
	if p.Response != nil {
		pv["response"] = p.Response
	}
	return pv
}

// noteView is what a model's notes compare of it with the model before it: what it is and what
// its API takes, not its name, dates, ids or pages, which differ from model to model by nature.
func noteView(s *Snapshot) map[string]any {
	facts := map[string]any{}
	for _, k := range []string{factKnowledgeCutoff, factTrainingCutoff, factContextWindow, factMaxInput, factMaxOutput,
		factInputModalities, factOutputModalities, factReasoning, factThinking, factReasoningEfforts, factDefaultEffort,
		factToolCalling, factStructuredOutputs, factSampling, factFeatures, factUnsupported, factTools, factEndpoints,
		factBatch, factLatency, factRegions} {
		if v, ok := s.Facts[k]; ok {
			facts[k] = v
		}
	}
	v := map[string]any{"facts": facts}
	if s.API != nil && s.API.Probes != nil {
		v["api"] = map[string]any{"probes": map[string]any{"params": s.API.Probes.Params}}
	}
	return roundTrip(v)
}

func roundTrip(v any) map[string]any {
	data, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

func useKey(u HarnessUse) string {
	k := u.Harness + "/" + u.Role
	if u.Label != "" {
		k += "/" + u.Label
	}
	return k
}

// compare is every change from a to b, titled and classified.
func compare(a, b map[string]any, names map[string]string) []refdiff.Change {
	var out []refdiff.Change
	for _, sec := range []string{"listed", "facts", "api", "harnesses", "docs"} {
		x, inA := a[sec]
		y, inB := b[sec]
		if !inA || !inB {
			// A section only one side has (no key then, a key now) is no change of the model.
			continue
		}
		for _, c := range refdiff.Values([]string{sec}, x, y) {
			c.ID = strings.Join(c.Path, "/")
			c.Category = sec
			c.Impact = impact(c)
			c.Title = title(c, names)
			out = append(out, c)
		}
	}
	return out
}

// impact is how much a change matters to someone choosing or calling the model: a refusal that
// was not there, or a limit lowered, breaks a caller; a fact or probe otherwise is notable; a
// page's text and the listing's detail are informational.
func impact(c refdiff.Change) string {
	switch c.Category {
	case "listed":
		return "notable"
	case "api":
		if len(c.Path) >= 4 && c.Path[2] == "params" {
			if before, ok := c.Before.(bool); ok && before && c.Path[len(c.Path)-1] == "accepted" {
				return "breaking"
			}
			return "notable"
		}
		if len(c.Path) >= 3 && c.Path[2] == "tokens" {
			return "notable"
		}
		return "informational"
	case "facts":
		if x, ok := num(c.Before); ok {
			if y, ok := num(c.After); ok && y < x {
				return "breaking"
			}
		}
		return "notable"
	case "harnesses":
		return "notable"
	}
	return "informational"
}

var factLabels = map[string]string{
	factName: "Name", factDescription: "Description", factStatus: "Status", factReleasedAt: "Release date",
	factKnowledgeCutoff: "Knowledge cutoff", factTrainingCutoff: "Training data cutoff", factContextWindow: "Context window",
	factMaxInput: "Max input", factMaxOutput: "Max output", factInputModalities: "Input", factOutputModalities: "Output",
	factReasoning: "Reasoning", factThinking: "Thinking", factReasoningEfforts: "Reasoning effort", factDefaultEffort: "Default effort",
	factToolCalling: "Tool calling", factStructuredOutputs: "Structured outputs", factSampling: "Sampling parameters",
	factOpenWeights: "Open weights", factFeatures: "Feature", factUnsupported: "Unsupported feature", factTools: "Built-in tool",
	factEndpoints: "Endpoint", factBatch: "Batch API", factLatency: "Latency", factRegions: "Region", factAliases: "Alias",
	factSnapshots: "Snapshot", factPlatformIDs: "Platform id", factDeprecatedAt: "Deprecated", factRetiresAt: "Retires",
	factRetiresNotBefore: "Retires no sooner than", factReplacement: "Replacement",
}

var roleLabels = map[string]string{"default": "its default model", "alias": "alias", "subagents": "its subagents' model",
	"compaction": "its compaction model", "fallback": "its fallback model", "side_request": "a side request's model"}

// title says a change in a line a person reads.
func title(c refdiff.Change, names map[string]string) string {
	p := c.Path
	switch c.Category {
	case "listed":
		if c.After == true {
			return "Listed by its vendor again"
		}
		return "No longer listed by its vendor"
	case "facts":
		label := factLabels[p[1]]
		if label == "" {
			label = p[1]
		}
		if len(p) == 3 {
			// An item of a list: an effort level, a feature, a tool.
			return fmt.Sprintf("%s %s: %s", label, map[string]string{"added": "added", "removed": "removed", "changed": "changed"}[c.Op], p[2])
		}
		return fmt.Sprintf("%s: %s", label, arrow(c, p[1]))
	case "api":
		if len(p) >= 4 && p[1] == "probes" && p[2] == "params" {
			name := p[3]
			switch {
			case c.Op == "added":
				return "Probed " + name + ": " + acceptance(c.After)
			case c.Op == "removed":
				return "No longer probed: " + name
			case len(p) == 5 && p[4] == "accepted":
				if c.After == true {
					return "API now accepts " + name
				}
				return "API now rejects " + name
			case len(p) == 5 && p[4] == "error":
				return "API's error for " + name + " changed"
			}
		}
		if len(p) >= 3 && p[1] == "probes" && p[2] == "tokens" {
			return "Token counts changed" + tokenNote(c)
		}
		if len(p) >= 3 && p[1] == "probes" && p[2] == "response" {
			return "Response " + strings.ReplaceAll(strings.Join(p[3:], " "), "_", " ") + ": " + arrow(c, "")
		}
		if len(p) >= 2 && p[1] == "listing" {
			return "API listing " + strings.Join(p[2:], ".") + ": " + arrow(c, "")
		}
		return strings.Join(p[1:], " ") + ": " + arrow(c, "")
	case "harnesses":
		if len(p) < 2 {
			break
		}
		parts := strings.SplitN(p[1], "/", 3)
		harness := names[parts[0]]
		if harness == "" {
			harness = parts[0]
		}
		role := roleLabels[parts[1]]
		if parts[1] == "alias" && len(parts) == 3 {
			role = "its " + parts[2] + " alias"
		}
		switch {
		case c.Op == "added":
			return harness + " uses it as " + role
		case c.Op == "removed":
			return harness + " no longer tracked using it as " + role
		case c.After == false:
			return harness + " stopped using it as " + role
		default:
			return harness + " uses it as " + role + " again"
		}
	case "docs":
		name := map[string]string{"overview": "Model page", "whats_new": "What's new page"}[p[1]]
		if name == "" {
			name = "Page " + p[1]
		}
		switch c.Op {
		case "added":
			return name + " published"
		case "removed":
			return name + " taken down"
		}
		if c.Stats != nil {
			return fmt.Sprintf("%s changed (+%d −%d chars)", name, c.Stats.AddedChars, c.Stats.RemovedChars)
		}
		return name + " changed"
	}
	return strings.Join(p, " ") + ": " + arrow(c, "")
}

func acceptance(v any) string {
	if m, ok := v.(map[string]any); ok {
		if m["accepted"] == true {
			return "accepted"
		}
		if e, ok := m["error"].(string); ok && e != "" {
			return "rejected (" + e + ")"
		}
		return "rejected"
	}
	return fmt.Sprint(v)
}

// arrow is "before → after", each said as fact says it.
func arrow(c refdiff.Change, fact string) string {
	switch c.Op {
	case "added":
		return "now " + say(c.After, fact)
	case "removed":
		return "no longer stated (was " + say(c.Before, fact) + ")"
	}
	return say(c.Before, fact) + " → " + say(c.After, fact)
}

// say is a value as a person reads it: a token count as 128K or 1M, a yes or no.
func say(v any, fact string) string {
	switch x := v.(type) {
	case nil:
		return "none"
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		switch fact {
		case factContextWindow, factMaxInput, factMaxOutput:
			return tokens(int64(x))
		}
		if x == math.Trunc(x) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+" "+say(x[k], ""))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// tokens is a token count as a vendor writes it: 128K, 1M, 1,048,576 where it is no round one.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000 && n%1_000 == 0:
		return fmt.Sprintf("%dK", n/1_000)
	}
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func tokenNote(c refdiff.Change) string {
	x, ok1 := num(c.Before)
	y, ok2 := num(c.After)
	if !ok1 || !ok2 || x == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s: %d → %d)", c.Path[len(c.Path)-1], int(x), int(y))
}

// tokenRatio is how many more (or fewer) tokens b counts than a over the corpus, in percent;
// ok false where either was not counted.
func tokenRatio(a, b *Snapshot) (float64, bool) {
	if a.API == nil || b.API == nil || a.API.Probes == nil || b.API.Probes == nil {
		return 0, false
	}
	ta, tb := 0, 0
	for _, t := range corpus {
		x, ok1 := a.API.Probes.Tokens[t.name]
		y, ok2 := b.API.Probes.Tokens[t.name]
		if !ok1 || !ok2 {
			return 0, false
		}
		ta += x
		tb += y
	}
	if ta == 0 {
		return 0, false
	}
	return 100 * float64(tb-ta) / float64(ta), true
}
