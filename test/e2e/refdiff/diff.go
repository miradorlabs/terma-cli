package refdiff

import (
	"fmt"
	"math"
	"reflect"
	"slices"
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
// backoff_ms is a measured wait, which a busy machine moves: whether the build honours
// retry-after, and how many times it tries, say what it does. A stored kind's file count and
// file names are the run's (logs rotate, a cleanup marker comes and goes); its format, records
// and tables are the build's. When an egress request came (before or after the model's) is the
// run's timing.
var skipKeys = map[string]bool{"sha256": true, "chars": true, "checked_at": true, "signed_at": true,
	"assessment": true, "resigned": true, "authority": true, "backoff_ms": true,
	"files": true, "paths": true, "more_paths": true, "when": true, "helped": true}

// measured are numbers a capture measures, which move a little from run to run: a change of
// less than measuredSlack of them is none.
var measured = map[string]bool{"size_bytes": true, "limit_chars": true, "stable_prefix_chars": true}

const measuredSlack = 0.05

// compare is every change from a to b.
func compare(a, b *Snapshot) []Change {
	at, bt := a.Tree(), b.Tree()
	var out []Change
	for _, sec := range Sections {
		if at[sec] == nil || bt[sec] == nil {
			continue
		}
		// A build whose interception failed somewhere (a TLS handshake the proxy lost) saw less
		// than one that did not: their egress is not compared. Both failing (a pinned host) is.
		if sec == "network" && interceptFailed(at) != interceptFailed(bt) {
			continue
		}
		if sec == "filesystem" {
			diffFilesystem(at[sec], bt[sec], &out) // part by part (filesystem.go)
			continue
		}
		if sec == "telemetry" {
			ta, aok := at[sec].(map[string]any)
			tb, bok := bt[sec].(map[string]any)
			if aok && bok {
				ta, tb = comparableTelemetry(ta, tb) // the parts both captures measured (telemetry.go)
				diffValue([]string{sec}, ta, tb, &out)
				continue
			}
		}
		if a, b, ok := configs(sec, at[sec], bt[sec]); ok {
			diffConfig(a, b, &out) // option by option (config.go)
			continue
		}
		diffValue([]string{sec}, at[sec], bt[sec], &out)
	}
	kept := out[:0]
	for _, c := range out {
		// Whether an event fired is the run's (SessionEnd races the exit): what a build supports
		// is hooks.supported, and an event's payload is compared where both fired.
		if c.Path[0] == "hooks" && len(c.Path) == 3 && c.Path[1] == "events" && c.Op != "changed" {
			continue
		}
		// How a build retries is measured in waits a busy machine moves and a run its probe cuts
		// short: kept in each snapshot, not yet compared until it reads the same run to run.
		if c.Path[0] == "behavior" && len(c.Path) > 1 && c.Path[1] == "retries" {
			continue
		}
		// A retry probe's status there in one build and not the other was cut short in one: not
		// observed, not a change of the build.
		if c.Path[0] == "behavior" && len(c.Path) == 4 && c.Path[1] == "retries" && c.Path[2] == "statuses" && c.Op != "changed" {
			continue
		}
		// An event a run reports only sometimes, missing where builds either side have it, is
		// not observed there.
		if c.Path[0] == "network" && len(c.Path) == 5 && c.Path[1] == "telemetry" && c.Path[3] == "events" && c.Op != "changed" &&
			(eventUnobserved(at, c.Path[2], c.Path[4]) || eventUnobserved(bt, c.Path[2], c.Path[4])) {
			continue
		}
		if c.Path[0] == "network" && len(c.Path) >= 4 && c.Path[1] == "telemetry" && c.Path[3] == "unobserved" {
			continue
		}
		// How many times a run reported a vendor telemetry event is the run's.
		if c.Path[0] == "network" && len(c.Path) == 6 && c.Path[1] == "telemetry" && c.Path[5] == "count" {
			continue
		}
		// Two builds whose hooks were probed different ways fired different tools in different
		// terminals: what their events were handed is the probe's difference, not the builds'.
		// What each supports is read from the build, and compared.
		if c.Path[0] == "hooks" && len(c.Path) >= 2 && (c.Path[1] == "events" && hookMethod(at) != hookMethod(bt) || c.Path[1] == "method") {
			continue
		}
		// How many times an event fired, and where the session was when it did, are the run's.
		if c.Path[0] == "hooks" && len(c.Path) == 4 && c.Path[1] == "events" && (c.Path[3] == "count" || c.Path[3] == "cwd") {
			continue
		}
		// So are the values an event was handed (its example payload, its variables' values):
		// what a build hands a hook is its keys and their kinds, and the variables' names.
		if c.Path[0] == "hooks" && len(c.Path) >= 4 && c.Path[1] == "events" && (c.Path[3] == "example" || c.Path[3] == "env_values") {
			continue
		}
		// A tool that became deferred keeps no schema in the request: that is its deferral (one
		// change), not its description and parameters removed.
		if c.Path[0] == "tools" && len(c.Path) >= 3 && c.Op == "removed" && deferredIn(bt, c.Path[1]) {
			continue
		}
		// A command only listed says its description where its lister does; one there and not
		// in the other build is not observed, not added or removed.
		if c.Path[0] == "cli" && len(c.Path) == 4 && c.Path[1] == "commands" && c.Path[3] == "description" && c.Op != "changed" {
			continue
		}
		if measured[c.Path[len(c.Path)-1]] && c.Op == "changed" && nearly(c.Before, c.After) {
			continue
		}
		// What a build stored is read from its files as the run left them: a database still
		// being written reads as no tables, a marker file comes and goes. A kind's structure
		// absent on one side is not observed; a kind is new or gone only with a structure.
		if c.Path[0] == "storage" && c.Op != "changed" && (len(c.Path) > 2 || !structured(c.Before, c.After)) {
			continue
		}
		// A capability checked on a platform for one build and not the other was not tested
		// there, not lost: only a result that changed is a change.
		if c.Path[0] == "support" && len(c.Path) == 3 && c.Path[1] == "capabilities" && c.Op != "changed" {
			continue
		}
		if c.Path[0] == "hooks" && hookEcho(c) {
			continue
		}
		// How many of its variables a desktop app passed on to its agent as it had them is the
		// machine's count, not the app's choice: what the app sets is cli.launch.env.
		if c.Path[0] == "cli" && len(c.Path) == 3 && c.Path[1] == "launch" && c.Path[2] == "inherited" {
			continue
		}
		kept = append(kept, c)
	}
	out = kept
	// A command only listed in one build (its own help not read) has no options to compare.
	helpedIn := func(t map[string]any, cmd string) bool {
		cli, _ := t["cli"].(map[string]any)
		cmds, _ := cli["commands"].(map[string]any)
		c, _ := cmds[cmd].(map[string]any)
		h, _ := c["helped"].(bool)
		return h
	}
	kept = out[:0]
	for _, c := range out {
		if c.Path[0] == "cli" && len(c.Path) > 3 && c.Path[1] == "commands" &&
			(c.Path[3] == "options" || c.Path[3] == "arguments" || c.Path[3] == "usage") &&
			(!helpedIn(at, c.Path[2]) || !helpedIn(bt, c.Path[2])) {
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
// variable itself, why the probe did not fire an event, and whether it trusted its hooks (notes
// of the capture's, which the events' own changes say).
func hookEcho(c Change) bool {
	if len(c.Path) > 1 && (c.Path[1] == "not_fired" || c.Path[1] == "trust") {
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
	case c.Category == "binary" && len(c.Path) > 3 && c.Path[1] == "app" && c.Path[2] == "signing" &&
		(c.Path[3] == "status" || c.Path[3] == "team_id" || c.Path[3] == "format" || c.Path[3] == "certificate" || c.Path[3] == "notarized"):
		c.Impact = "notable" // the app's signer, certificate or notarization
	case c.Category == "binary" && len(c.Path) == 3 && c.Path[1] == "agent" && (c.Path[2] == "version" || c.Path[2] == "harness" || c.Path[2] == "source"):
		c.Impact = "notable" // a desktop app that runs another build of its agent
	case c.Category == "cli" && len(c.Path) >= 2 && c.Path[1] == "launch":
		c.Impact = "notable" // how a desktop app starts its agent
	case c.Category == "cli" && len(c.Path) >= 3 && c.Path[1] == "commands" &&
		(c.Op == "removed" && (len(c.Path) == 3 || len(c.Path) == 5) || last == "default" || len(c.Path) > 5 && c.Path[5] == "choices"):
		c.Impact = "notable" // a command or flag gone, a default moved, an allowed value added or dropped
	case c.Category == "config":
		c.Impact = configImpact(c)
	case c.Category == "filesystem":
		c.Impact = filesystemImpact(c)
	case notableCategories[c.Category]:
		c.Impact = "notable"
	}
	c.Title = title(c, labels)
}

// genericSegments name a place, not the thing changed: a changelog that says "keys" says
// nothing of a key.
var genericSegments = map[string]bool{"keys": true, "surfaces": true, "system": true, "context": true, "text": true,
	"description": true, "input_schema": true, "events": true, "flags": true, "subcommands": true, "help": true,
	"headers": true, "betas": true, "params": true, "egress": true, "kinds": true, "class": true, "properties": true,
	"breakpoints": true, "statuses": true, "result": true, "status": true, "capabilities": true, "default": true,
	"body_keys": true, "identifiers": true, "aliases": true, "prices": true, "type": true, "required": true,
	"supported": true, "control": true, "env_paths": true, "matcher": true, "fired": true, "count": true,
	"options": true, "enum": true, "deprecated": true, "env_vars": true, "settings_keys": true, "source": true, "env_source": true}

// changelogWords reads a changelog's words alike, both sides: a changelog says gpt-6.1-sol as
// "GPT-6.1 Sol", and a field's name as words, so it is lower case, its dashes and underscores
// spaces.
var changelogWords = strings.NewReplacer("-", " ", "_", " ")

// changelogText is a changelog as documented reads it: lower case, dashes and underscores
// spaces. New makes it once for every change it checks.
func changelogText(changelog string) string {
	return changelogWords.Replace(strings.ToLower(changelog))
}

// documented reports whether a changelog mentions c: the thing it names (a tool, a field, a
// flag, a model), or for a prompt's change, the prompt. text is the changelog as changelogText
// reads it.
func documented(c Change, text string) bool {
	words := changelogWords
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
		text := changelogText(changelog.Text)
		for i := range d.Changes {
			doc := documented(d.Changes[i], text)
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
		// A planted credential the build now touches is read before the rest of its kind.
		if di, dj := isDecoy(cs[i]), isDecoy(cs[j]); di != dj {
			return di
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
	slices.SortStableFunc(versions, func(a, b string) int { return VersionCompare(b, a) })
	// The notes joined are bounded as one note is: each part first, then the whole, which is
	// truncated where any part was or the join was cut.
	var note *ChangelogNote
	if len(versions) > 0 {
		var sb strings.Builder
		cut := false
		for _, v := range versions {
			part := notes[v].Bounded()
			cut = cut || part.Truncated
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", v, strings.TrimSpace(part.Text))
		}
		note = BoundedNote(notes[versions[0]].URL, strings.TrimSpace(sb.String()))
		note.Truncated = note.Truncated || cut
	}
	return New(from, to, note, now)
}

// nearly reports two measurements within measuredSlack of each other.
func nearly(a, b any) bool {
	x, okA := a.(float64)
	y, okB := b.(float64)
	if !okA || !okB || x == 0 && y == 0 {
		return okA && okB
	}
	return math.Abs(x-y) <= measuredSlack*math.Max(math.Abs(x), math.Abs(y))
}

// structured reports a stored kind, of either side, with a structure to compare: its records,
// keys or tables.
func structured(vs ...any) bool {
	for _, v := range vs {
		m, _ := v.(map[string]any)
		for _, k := range []string{"record_types", "keys", "tables"} {
			if m[k] != nil {
				return true
			}
		}
	}
	return false
}

// deferredIn reports a tool the snapshot names as deferred.
func deferredIn(t map[string]any, name string) bool {
	tools, _ := t["tools"].([]any)
	for _, x := range tools {
		if m, _ := x.(map[string]any); keyOf(m) == name {
			d, _ := m["deferred"].(bool)
			return d
		}
	}
	return false
}

func deferredValue(v any) bool {
	m, _ := v.(map[string]any)
	d, _ := m["deferred"].(bool)
	return d
}

// interceptFailed reports a snapshot whose egress has a request the proxy could not read.
func interceptFailed(t map[string]any) bool {
	net, _ := t["network"].(map[string]any)
	egress, _ := net["egress"].([]any)
	for _, e := range egress {
		if m, _ := e.(map[string]any); m["error"] != nil {
			return true
		}
	}
	return false
}

// eventUnobserved reports an event a snapshot's run missed at an endpoint.
func eventUnobserved(t map[string]any, endpoint, name string) bool {
	net, _ := t["network"].(map[string]any)
	tel, _ := net["telemetry"].(map[string]any)
	ep, _ := tel[endpoint].(map[string]any)
	list, _ := ep["unobserved"].([]any)
	for _, v := range list {
		if v == name {
			return true
		}
	}
	if ss, ok := ep["unobserved"].([]string); ok {
		for _, v := range ss {
			if v == name {
				return true
			}
		}
	}
	return false
}

// hookMethod is how a snapshot's hooks were probed: its method, else 2 where its events have
// example payloads (the first probe to publish them), else 1.
func hookMethod(sections map[string]any) int {
	hooks, _ := sections["hooks"].(map[string]any)
	if m, ok := hooks["method"].(float64); ok {
		return int(m)
	}
	events, _ := hooks["events"].(map[string]any)
	for _, ev := range events {
		if e, _ := ev.(map[string]any); e != nil && e["example"] != nil {
			return 2
		}
	}
	return 1
}
