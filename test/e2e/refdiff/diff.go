package refdiff

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Comparing two snapshots of a harness. Each section both have is compared as JSON: maps key
// by key, a list of things with names by name (tools, capabilities, egress), a list of words
// as a set (betas, flags, env vars), a long text line by line. A section only one has is not
// compared: an older snapshot taken before a capture existed is no evidence the newer build
// added everything that capture sees. Each difference is classified: its category is its
// section, its impact what it means for someone relying on the harness, terma included.

// skipKeys are derived from what they sit beside, and would only repeat a text change.
// checked_at is when a signature was checked, not what it says.
// signed_at differs from build to build by nature; assessment, resigned and authority say again
// what status and the certificate say.
var skipKeys = map[string]bool{"sha256": true, "chars": true, "checked_at": true, "signed_at": true,
	"assessment": true, "resigned": true, "authority": true}

// compare is every change from a to b.
func compare(a, b *Snapshot) []Change {
	at, bt := a.Tree(), b.Tree()
	var out []Change
	for _, sec := range Sections {
		if at[sec] == nil || bt[sec] == nil {
			continue
		}
		diffValue([]string{sec}, at[sec], bt[sec], &out)
	}
	kept := out[:0]
	for _, c := range out {
		// A capability checked on a platform for one build and not the other was not tested
		// there, not lost: only a result that changed is a change.
		if c.Path[0] == "support" && len(c.Path) == 3 && c.Path[1] == "capabilities" && c.Op != "changed" {
			continue
		}
		if c.Path[0] == "hooks" && hookEcho(c) {
			continue
		}
		kept = append(kept, c)
	}
	out = kept
	labels := capabilityLabels(bt)
	for i := range out {
		classify(&out[i], labels)
	}
	return out
}

// hookEcho reports whether a change in the hooks section only says again what another says: a
// payload field's kind or an environment variable's path added or removed with the field or
// variable itself, and why the probe did not fire an event (a note of the capture's, which the
// event's own change says).
func hookEcho(c Change) bool {
	if len(c.Path) > 1 && c.Path[1] == "not_fired" {
		return true
	}
	return len(c.Path) == 5 && c.Path[1] == "events" && (c.Path[3] == "kinds" || c.Path[3] == "env_paths") && c.Op != "changed"
}

// capabilityLabels are the labels of the capabilities a snapshot's support lists, by their key
// in it (id@platform).
func capabilityLabels(t map[string]any) map[string]string {
	out := map[string]string{}
	sup, _ := t["support"].(map[string]any)
	caps, _ := sup["capabilities"].([]any)
	for _, c := range caps {
		m, _ := c.(map[string]any)
		if l, _ := m["label"].(string); l != "" {
			out[keyOf(m)] = l
		}
	}
	return out
}

// keyOf is what names a list item, "" for one without a name.
func keyOf(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	switch {
	case str("host") != "":
		return strings.TrimSpace(str("method") + " " + str("host") + str("path"))
	case str("id") != "" && str("platform") != "":
		return str("id") + "@" + str("platform")
	case str("name") != "" && str("namespace") != "":
		return str("namespace") + "/" + str("name")
	case str("name") != "":
		return str("name")
	case str("method") != "" && str("path") != "":
		k := str("method") + " " + str("path")
		if model := str("model"); model != "" {
			k += " " + model
		}
		return k
	case str("at") != "":
		return str("at")
	case str("id") != "":
		return str("id")
	}
	return ""
}

// diffValue appends the changes from a to b at path.
func diffValue(path []string, a, b any, out *[]Change) {
	if reflect.DeepEqual(a, b) {
		return
	}
	// A signing certificate is one thing: a new one is one change, whatever of it differs.
	if len(path) >= 2 && path[len(path)-1] == "certificate" && path[len(path)-2] == "signing" {
		*out = append(*out, Change{Op: "changed", Path: path, Before: a, After: b})
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			break
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		for _, k := range sortedKeys(keys) {
			if skipKeys[k] {
				continue
			}
			x, inA := av[k]
			y, inB := bv[k]
			p := append(append([]string{}, path...), k)
			switch {
			case !inA:
				*out = append(*out, Change{Op: "added", Path: p, After: y})
			case !inB:
				*out = append(*out, Change{Op: "removed", Path: p, Before: x})
			default:
				diffValue(p, x, y, out)
			}
		}
		return
	case []any:
		bv, ok := b.([]any)
		if !ok {
			break
		}
		if diffList(path, av, bv, out) {
			return
		}
	case string:
		if bs, ok := b.(string); ok && (isText(av) || isText(bs)) {
			d, stats := unifiedDiff(av, bs)
			*out = append(*out, Change{Op: "changed", Path: path, TextDiff: &d, Stats: &stats})
			return
		}
	}
	*out = append(*out, Change{Op: "changed", Path: path, Before: a, After: b})
}

// isText reports a string compared as text: more than a line, or long.
func isText(s string) bool { return strings.Contains(s, "\n") || len(s) > 120 }

// diffList compares two lists by what they hold, and reports whether it could; a list it
// cannot reads as changed whole.
func diffList(path []string, a, b []any, out *[]Change) bool {
	all := append(append([]any{}, a...), b...)
	if len(all) == 0 {
		return true
	}
	keyed, words := true, true
	for _, v := range all {
		if keyOf(v) == "" {
			keyed = false
		}
		if _, ok := v.(string); !ok {
			words = false
		}
	}
	switch {
	case keyed:
		am, bm := map[string]any{}, map[string]any{}
		var order []string
		for _, v := range a {
			k := keyOf(v)
			am[k] = v
			order = append(order, k)
		}
		for _, v := range b {
			k := keyOf(v)
			if _, ok := am[k]; !ok {
				order = append(order, k)
			}
			bm[k] = v
		}
		for _, k := range order {
			x, inA := am[k]
			y, inB := bm[k]
			p := append(append([]string{}, path...), k)
			switch {
			case !inA:
				*out = append(*out, Change{Op: "added", Path: p, After: y})
			case !inB:
				*out = append(*out, Change{Op: "removed", Path: p, Before: x})
			default:
				diffValue(p, x, y, out)
			}
		}
		return true
	case words:
		as, bs := map[string]bool{}, map[string]bool{}
		for _, v := range a {
			as[v.(string)] = true
		}
		for _, v := range b {
			bs[v.(string)] = true
		}
		for _, v := range b {
			if s := v.(string); !as[s] {
				*out = append(*out, Change{Op: "added", Path: append(append([]string{}, path...), s), After: s})
			}
		}
		for _, v := range a {
			if s := v.(string); !bs[s] {
				*out = append(*out, Change{Op: "removed", Path: append(append([]string{}, path...), s), Before: s})
			}
		}
		return true
	}
	// Maps without names (a prompt's blocks) compare by position.
	for _, v := range all {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	for i := range max(len(a), len(b)) {
		p := append(append([]string{}, path...), fmt.Sprint(i))
		switch {
		case i >= len(a):
			*out = append(*out, Change{Op: "added", Path: p, After: b[i]})
		case i >= len(b):
			*out = append(*out, Change{Op: "removed", Path: p, Before: a[i]})
		default:
			diffValue(p, a[i], b[i], out)
		}
	}
	return true
}

// notableCategories are what a person relying on the harness's behaviour would want to know
// of: what the model is asked and how.
var notableCategories = map[string]bool{"prompt": true, "tools": true, "model": true, "caching": true,
	"network": true, "behavior": true, "side_requests": true}

// classify fills a change's id, category, impact and title; labels name the capabilities.
func classify(c *Change, labels map[string]string) {
	c.Category = c.Path[0]
	c.ID = strings.Join(c.Path, ".")
	c.Impact = "informational"
	last := c.Path[len(c.Path)-1]
	switch {
	case c.Category == "support" && last == "result":
		if c.Before == "pass" && c.After == "fail" {
			c.Impact = "breaking"
		} else {
			c.Impact = "notable"
		}
		if len(c.Path) >= 3 {
			id, _, _ := strings.Cut(c.Path[2], "@")
			c.Terma = &TermaEffect{Capabilities: []string{id}}
		}
	case c.Category == "support" && last == "status":
		if c.Before == "supported" && (c.After == "degraded" || c.After == "unsupported") {
			c.Impact = "breaking"
		} else {
			c.Impact = "notable"
		}
	case c.Category == "support" && len(c.Path) == 3 && c.Op != "changed":
		c.Impact = "informational"
	case c.Category == "telemetry" && c.Op == "removed" && len(c.Path) == 3:
		c.Impact = "degrading" // a surface no longer sent
	case c.Category == "telemetry" && c.Op == "removed":
		c.Impact = "notable"
	case c.Category == "hooks" && c.Op == "removed" && len(c.Path) == 3:
		c.Impact = "degrading" // a hook event no longer supported, or no longer fired
	case c.Category == "hooks" && c.Op == "added" && len(c.Path) == 3 && c.Path[1] == "supported":
		c.Impact = "notable" // a new hook event
	case c.Category == "hooks" && c.Op == "changed" && len(c.Path) == 5 && c.Path[3] == "control":
		c.Impact = "notable" // what a hook's answer does
	case c.Category == "hooks" && c.Op == "removed":
		c.Impact = "notable"
	case c.Category == "request" && len(c.Path) > 1 && (c.Path[1] == "betas" || c.Path[1] == "endpoint" || c.Path[1] == "identifiers"):
		c.Impact = "notable"
	case c.Category == "binary" && len(c.Path) > 2 && c.Path[1] == "signing" &&
		(c.Path[2] == "status" || c.Path[2] == "team_id" || c.Path[2] == "format" || c.Path[2] == "certificate"):
		c.Impact = "notable" // a new signer, a new certificate, or one revoked
	case notableCategories[c.Category]:
		c.Impact = "notable"
	}
	c.Title = title(c, labels)
}

// title is a change said in a line.
func title(c *Change, labels map[string]string) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	verb := map[string]string{"added": "added", "removed": "removed", "changed": "changed"}[c.Op]
	short := func(v any) string {
		switch x := v.(type) {
		case nil:
			return "none"
		case string:
			if len(x) > 60 {
				return x[:57] + "…"
			}
			return x
		case float64:
			return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
		}
		data, _ := json.Marshal(v)
		if len(data) > 60 {
			return string(data[:57]) + "…"
		}
		return string(data)
	}
	arrow := func() string { return short(c.Before) + " → " + short(c.After) }
	stats := ""
	if c.Stats != nil {
		stats = fmt.Sprintf(" (+%d −%d chars)", c.Stats.AddedChars, c.Stats.RemovedChars)
	}
	switch c.Category {
	case "prompt":
		what := map[string]string{"system": "System prompt", "context": "Injected context", "interactive": "Interactive system prompt"}[at(1)]
		if what == "" {
			what = "Prompt " + at(1)
		}
		if at(1) == "interactive" && at(2) == "context" {
			what = "Interactive session's injected context"
		}
		if c.Op != "changed" && (at(1) == "system" || at(1) == "context") && len(p) == 3 {
			return fmt.Sprintf("%s block %s", what, verb)
		}
		return what + " " + verb + stats
	case "tools":
		switch {
		case len(p) == 2 && c.Op == "added":
			return "New tool: " + at(1)
		case len(p) == 2 && c.Op == "removed":
			return "Tool removed: " + at(1)
		case at(2) == "description":
			return at(1) + " tool description " + verb + stats
		case at(2) == "input_schema" && at(3) == "properties" && len(p) == 5:
			return fmt.Sprintf("%s tool parameter %s %s", at(1), at(4), verb)
		case at(2) == "input_schema" && at(3) == "properties" && len(p) > 5:
			return fmt.Sprintf("%s tool parameter %s: %s %s%s", at(1), at(4), strings.Join(p[5:], "."), verb, stats)
		case at(2) == "input_schema":
			return at(1) + " tool parameters " + verb
		}
		return at(1) + " tool " + strings.Join(p[2:], ".") + " " + verb
	case "model":
		switch at(1) {
		case "default":
			return "Default model: " + arrow()
		case "max_tokens":
			return "Max output tokens: " + arrow()
		case "aliases":
			return fmt.Sprintf("Model alias %q: %s", at(2), arrow())
		case "prices":
			return "Built-in price for " + at(2) + " " + verb
		case "params":
			if len(p) == 3 && c.Op != "changed" {
				return "Model parameter " + at(2) + " " + verb
			}
			return "Model parameter " + strings.Join(p[2:], ".") + ": " + arrow()
		}
	case "caching":
		if at(1) == "breakpoints" {
			return "Cache breakpoint " + strings.Join(p[2:], ".") + " " + verb
		}
		return "Caching " + strings.Join(p[1:], ".") + ": " + arrow()
	case "request":
		switch at(1) {
		case "betas":
			if c.Op == "added" {
				return "New beta: " + at(2)
			}
			return "Beta dropped: " + at(2)
		case "headers":
			if c.Op == "changed" && c.TextDiff != nil {
				return "Header " + at(2) + " changed" + stats
			}
			if c.Op == "changed" {
				return "Header " + at(2) + ": " + arrow()
			}
			return "Header " + at(2) + " " + verb
		case "identifiers":
			if c.Op == "added" {
				return "Now sends identifier " + at(2)
			}
			return "No longer sends identifier " + at(2)
		case "body_keys":
			return "Request field " + at(2) + " " + verb
		}
		return "Request " + strings.Join(p[1:], ".") + " " + verb
	case "side_requests":
		if len(p) == 2 {
			return "Side request " + at(1) + " " + verb
		}
		return "Side request " + at(1) + " " + strings.Join(p[2:], ".") + " " + verb
	case "behavior":
		return "Behaviour: " + strings.Join(p[1:], " ") + " " + verb + valueNote(c)
	case "telemetry":
		if at(1) == "surfaces" {
			switch {
			case len(p) == 3 && c.Op == "added":
				return fmt.Sprintf("New telemetry surface: %s%s", at(2), keyCount(c.After))
			case len(p) == 3 && c.Op == "removed":
				return "Telemetry surface no longer sent: " + at(2)
			case at(3) == "keys" && len(p) == 5 && c.Op == "added":
				return fmt.Sprintf("New telemetry field: %s on %s", at(4), at(2))
			case at(3) == "keys" && len(p) == 5 && c.Op == "removed":
				return fmt.Sprintf("Telemetry field no longer sent: %s on %s", at(4), at(2))
			case at(3) == "keys" && len(p) > 5:
				return fmt.Sprintf("Telemetry field %s on %s: %s %s", at(4), at(2), strings.Join(p[5:], "."), arrow())
			}
		}
		return "Telemetry " + strings.Join(p[1:], ".") + " " + verb + valueNote(c)
	case "hooks":
		return hookTitle(c, verb, arrow)
	case "cli":
		switch at(1) {
		case "flags":
			if c.Op == "added" {
				return "New flag: " + at(2)
			}
			return "Flag removed: " + at(2)
		case "subcommands":
			if c.Op == "added" {
				return "New subcommand: " + at(2)
			}
			return "Subcommand removed: " + at(2)
		case "help":
			// "exec --help" → `codex exec --help`, as a person types it; the build's own help by its
			// flag alone.
			return "Help text of `" + at(2) + "` " + verb + stats
		}
	case "config":
		what := map[string]string{"env_vars": "environment variable", "settings_keys": "setting"}[at(1)]
		if what != "" && len(p) == 3 {
			if c.Op == "added" {
				return "New " + what + ": " + at(2)
			}
			return strings.ToUpper(what[:1]) + what[1:] + " no longer read: " + at(2)
		}
	case "network":
		if len(p) == 3 {
			if c.Op == "added" {
				return "New network request: " + at(2)
			}
			if c.Op == "removed" {
				return "Network request no longer made: " + at(2)
			}
		}
		return "Network request " + at(2) + " " + strings.Join(p[3:], ".") + " " + verb
	case "storage":
		return "On-disk " + strings.Join(p[1:], " ") + " " + verb + valueNote(c)
	case "binary":
		if at(1) == "signing" {
			switch at(2) {
			case "status":
				if c.After == "revoked" {
					return "Signing certificate revoked"
				}
				return "Code signature status: " + arrow()
			case "certificate":
				return "Signed with a different certificate"
			case "team_id":
				return "Signed by a different team: " + arrow()
			case "format":
				return "Code signature: " + arrow()
			}
			return "Code signature " + strings.Join(p[2:], ".") + ": " + arrow()
		}
		return "Binary " + strings.Join(p[1:], ".") + ": " + arrow()
	case "support":
		if len(p) >= 4 && at(3) == "result" {
			id, platform, _ := strings.Cut(at(2), "@")
			label := id
			if l := labels[at(2)]; l != "" {
				label = l
			}
			if m, ok := c.After.(string); ok && m == "fail" {
				return fmt.Sprintf("Terma: %s now fails on %s", label, platform)
			}
			if c.After == "pass" {
				return fmt.Sprintf("Terma: %s now passes on %s", label, platform)
			}
			return fmt.Sprintf("Terma: %s now %s on %s", label, short(c.After), platform)
		}
		if at(1) == "status" {
			return "Terma support: " + arrow()
		}
	}
	return strings.Join(p, ".") + " " + verb + valueNote(c)
}

// hookTitle is a change to the hooks section said in a line: the events a build knows
// (supported), and what each that fired was handed and did with a hook's answer (events).
func hookTitle(c *Change, verb string, arrow func() string) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	event := at(2)
	switch {
	case at(1) == "supported" && c.Op == "added":
		return "New hook event: " + event
	case at(1) == "supported" && c.Op == "removed":
		return "Hook event no longer supported: " + event
	case at(1) == "source":
		return "Hook events read from: " + arrow()
	case at(1) != "events":
	case len(p) == 3 && c.Op == "added":
		return "Hook event now fired: " + event
	case len(p) == 3 && c.Op == "removed":
		return "Hook event no longer fired: " + event
	case len(p) == 5 && at(3) == "keys":
		return fmt.Sprintf("%s hook payload field %s %s", event, at(4), verb)
	case len(p) == 5 && at(3) == "kinds":
		return fmt.Sprintf("%s hook payload field %s: %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "env":
		return fmt.Sprintf("%s hook environment variable %s %s", event, at(4), verb)
	case len(p) == 5 && at(3) == "env_paths":
		return fmt.Sprintf("%s hook environment variable %s: %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "control" && c.Op == "changed":
		return fmt.Sprintf("%s hook control (%s): %s", event, at(4), arrow())
	case len(p) == 5 && at(3) == "control":
		return fmt.Sprintf("%s hook control (%s) %s", event, at(4), verb)
	case len(p) == 4 && at(3) == "matcher":
		return fmt.Sprintf("%s hook takes a matcher: %s", event, arrow())
	case len(p) == 4 && at(3) == "cwd":
		return fmt.Sprintf("%s hook working directory: %s", event, arrow())
	case len(p) == 4 && c.Op == "changed":
		return fmt.Sprintf("%s hook %s: %s", event, at(3), arrow())
	}
	return "Hook " + strings.Join(p[1:], ".") + " " + verb
}

// valueNote is ": before → after" for a short change of value, nothing otherwise.
func valueNote(c *Change) string {
	if c.Op != "changed" || c.TextDiff != nil {
		return ""
	}
	b, _ := json.Marshal(c.Before)
	a, _ := json.Marshal(c.After)
	if len(a)+len(b) > 80 {
		return ""
	}
	return ": " + string(b) + " → " + string(a)
}

func keyCount(v any) string {
	m, _ := v.(map[string]any)
	keys, _ := m["keys"].(map[string]any)
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d fields)", len(keys))
}

// genericSegments name a place, not the thing changed: a changelog that says "keys" says
// nothing of a key.
var genericSegments = map[string]bool{"keys": true, "surfaces": true, "system": true, "context": true, "text": true,
	"description": true, "input_schema": true, "events": true, "flags": true, "subcommands": true, "help": true,
	"headers": true, "betas": true, "params": true, "egress": true, "kinds": true, "class": true, "properties": true,
	"breakpoints": true, "statuses": true, "result": true, "status": true, "capabilities": true, "default": true,
	"body_keys": true, "identifiers": true, "aliases": true, "prices": true, "type": true, "required": true,
	"supported": true, "control": true, "env_paths": true, "matcher": true, "fired": true, "count": true}

// documented reports whether changelog mentions c: the thing it names (a tool, a field, a
// flag, a model), or for a prompt's change, the prompt.
func documented(c Change, changelog string) bool {
	// A changelog says gpt-6.1-sol as "GPT-6.1 Sol", and a field's name as words: dashes and
	// underscores read as spaces on both sides.
	words := strings.NewReplacer("-", " ", "_", " ")
	text := words.Replace(strings.ToLower(changelog))
	if c.Category == "prompt" {
		return strings.Contains(text, "system prompt")
	}
	// A text's change (a tool's description) is said only where the changelog names both the
	// thing and its wording: a changelog naming Bash for a fix says nothing of its description.
	if c.TextDiff != nil && len(c.Path) > 1 {
		owner := words.Replace(strings.ToLower(c.Path[1]))
		if _, name, ok := strings.Cut(owner, "/"); ok {
			owner = name // a namespaced tool, by its own name
		}
		return strings.Contains(text, owner) && (strings.Contains(text, "description") || strings.Contains(text, "prompt") || strings.Contains(text, "instruction"))
	}
	var tokens []string
	for _, seg := range c.Path[1:] {
		if !genericSegments[seg] {
			tokens = append(tokens, seg)
		}
	}
	for _, v := range []any{c.Before, c.After} {
		if s, ok := v.(string); ok && len(s) < 60 && !strings.Contains(s, " ") {
			tokens = append(tokens, s)
		}
	}
	for _, t := range tokens {
		t = strings.ToLower(strings.TrimLeft(t, "-"))
		// The last part of a dotted name is how a changelog says it ("token.usage" → "usage" is
		// too common: keep the whole, and the last part where it is long enough).
		cands := []string{t}
		if i := strings.LastIndexAny(t, "./@ "); i >= 0 && len(t)-i > 6 {
			cands = append(cands, t[i+1:])
		}
		for _, cand := range cands {
			if cand = words.Replace(cand); len(cand) >= 4 && strings.Contains(text, cand) {
				return true
			}
		}
	}
	return false
}

// New is the diff from a to b, its changes summarized, and checked against the changelog
// where there is one.
func New(a, b *Snapshot, changelog *ChangelogNote, now time.Time) *Diff {
	d := &Diff{SchemaVersion: SchemaVersion, Harness: b.Harness, From: a.Version, To: b.Version, GeneratedAt: now,
		Changelog: changelog, Changes: compare(a, b)}
	if d.Changes == nil {
		d.Changes = []Change{}
	}
	d.Summary = summarize(d.Changes)
	if changelog != nil {
		undocumented := 0
		for i := range d.Changes {
			doc := documented(d.Changes[i], changelog.Text)
			d.Changes[i].Documented = &doc
			if !doc && d.Changes[i].Category != "support" {
				undocumented++
			}
		}
		d.Summary.Undocumented = &undocumented
	}
	return d
}

func summarize(changes []Change) DiffSummary {
	s := DiffSummary{Changes: len(changes), Categories: map[string]int{},
		Impact: map[string]int{"breaking": 0, "degrading": 0, "notable": 0, "informational": 0}}
	for _, c := range changes {
		s.Categories[c.Category]++
		s.Impact[c.Impact]++
	}
	return s
}

// Highlights are a diff's few changes a person reads first: the most severe, then the
// categories people care most about, one line each.
func Highlights(d *Diff, n int) []string {
	rank := map[string]int{"breaking": 0, "degrading": 1, "notable": 2, "informational": 3}
	order := map[string]int{}
	for i, s := range Sections {
		order[s] = i
	}
	cs := append([]Change{}, d.Changes...)
	sort.SliceStable(cs, func(i, j int) bool {
		if rank[cs[i].Impact] != rank[cs[j].Impact] {
			return rank[cs[i].Impact] < rank[cs[j].Impact]
		}
		return order[cs[i].Category] < order[cs[j].Category]
	})
	out := []string{}
	for _, c := range cs {
		if len(out) == n {
			break
		}
		out = append(out, c.Title)
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

// CompareRange is the diff between any two snapshots of a harness, from to to, however many
// versions apart: what differs between them, net. A change is documented where the vendor's
// changelog of any version between them (after from, up to to) mentions it; notes are those
// changelogs by version.
func CompareRange(from, to *Snapshot, notes map[string]ChangelogNote, now time.Time) *Diff {
	lo, hi := from.Version, to.Version
	if VersionLess(hi, lo) {
		lo, hi = hi, lo
	}
	versions := make([]string, 0, len(notes))
	for v := range notes {
		if VersionLess(lo, v) && !VersionLess(hi, v) {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return VersionLess(versions[j], versions[i]) })
	var note *ChangelogNote
	if len(versions) > 0 {
		var sb strings.Builder
		for _, v := range versions {
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", v, strings.TrimSpace(notes[v].Text))
		}
		note = &ChangelogNote{URL: notes[versions[0]].URL, Text: strings.TrimSpace(sb.String())}
	}
	return New(from, to, note, now)
}
