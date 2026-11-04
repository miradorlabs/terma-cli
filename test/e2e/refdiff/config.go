package refdiff

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// Comparing two builds' configuration: their options key by key (config.options, by dotted
// path), and the environment variables they read. Each option's type, default, values,
// description and deprecation is a change of its own, so a diff reads as a list ("New setting:
// x", "Default of y: a → b"). Two builds whose options were read from different kinds of
// source (a JSON schema, a struct, a bundle) are not compared option by option: a source that
// says more is no evidence the build added what it says; nor variables read differently (the
// names a binary's strings hold, those its code reads). config.source names its build's tag,
// which is no change.

// configs are two snapshots' config sections, where sec is the config section.
func configs(sec string, a, b any) (map[string]any, map[string]any, bool) {
	x, okA := a.(map[string]any)
	y, okB := b.(map[string]any)
	return x, y, sec == "config" && okA && okB
}

// diffConfig appends the changes from a's config to b's.
func diffConfig(a, b map[string]any, out *[]Change) {
	path := func(p ...string) []string { return append([]string{"config"}, p...) }
	srcA, srcB := sourceKind(a["source"]), sourceKind(b["source"])
	if srcA != srcB && a["source"] != nil && b["source"] != nil {
		*out = append(*out, Change{Op: "changed", Path: path("source"), Before: srcA, After: srcB})
	}
	optsA, okA := a["options"].(map[string]any)
	optsB, okB := b["options"].(map[string]any)
	first := len(*out)
	defer func() { *out = append((*out)[:first], groupEnvVars(collapseProfiles((*out)[first:]))...) }()
	// A schema read from minified code resolves a nested schema on one build and not the next:
	// its top-level settings are compared item by item, and of a nested one only what changed in
	// both. Its types, which a minified name resolved to its nearest definition says, are not.
	approx := approximate(srcA)
	if okA && okB && srcA == srcB {
		for _, k := range sortedKeys(union(optsA, optsB)) {
			x, inA := optsA[k]
			y, inB := optsB[k]
			if approx && strings.ContainsAny(k, ".[") && (!inA || !inB) {
				continue
			}
			if approx && inA && inB {
				x, y = withoutType(x), withoutType(y)
			}
			switch {
			case !inA && !unread(optsA, k):
				*out = append(*out, Change{Op: "added", Path: path("options", k), After: y})
			case !inB && !unread(optsB, k):
				*out = append(*out, Change{Op: "removed", Path: path("options", k), Before: x})
			case !inA || !inB:
			default:
				diffOption(path("options", k), x, y, out)
			}
		}
	}
	envA, envB := envKind(a), envKind(b)
	if envA != envB && a["env_vars"] != nil && b["env_vars"] != nil {
		*out = append(*out, Change{Op: "changed", Path: path("env_source"), Before: envA, After: envB})
	}
	for _, k := range sortedKeys(union(a, b)) {
		switch k {
		case "source", "options", "env_source":
			continue
		case "env_vars", "env_vars_uncertain":
			if envA != envB {
				continue
			}
		case "settings_keys":
			if okA && okB {
				continue // what the options say again
			}
		}
		x, inA := a[k]
		y, inB := b[k]
		if inA && inB {
			diffValue(path(k), x, y, out)
		}
	}
}

// unread reports whether opts has key's table but none of its keys: a table whose keys its
// reader could not follow (a schema a minified bundle builds at run time), not one emptied.
func unread(opts map[string]any, key string) bool {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return false
	}
	parent := strings.TrimSuffix(key[:i], "[]")
	if _, ok := opts[parent]; !ok {
		return false
	}
	for k := range opts {
		if strings.HasPrefix(k, parent+".") || strings.HasPrefix(k, parent+"[].") {
			return false
		}
	}
	return true
}

// diffOption appends the changes of one option.
func diffOption(path []string, a, b any, out *[]Change) {
	x, _ := a.(map[string]any)
	y, _ := b.(map[string]any)
	if reflect.DeepEqual(x, y) {
		return
	}
	at := func(k string) []string { return append(append([]string{}, path...), k) }
	for _, k := range []string{"type", "default", "deprecated"} {
		if !reflect.DeepEqual(x[k], y[k]) {
			*out = append(*out, Change{Op: "changed", Path: at(k), Before: x[k], After: y[k]})
		}
	}
	ea, _ := x["enum"].([]any)
	eb, _ := y["enum"].([]any)
	has := func(vs []any, v any) bool {
		return slices.ContainsFunc(vs, func(w any) bool { return reflect.DeepEqual(v, w) })
	}
	for _, v := range eb {
		if !has(ea, v) {
			*out = append(*out, Change{Op: "added", Path: append(at("enum"), fmt.Sprint(v)), After: v})
		}
	}
	for _, v := range ea {
		if !has(eb, v) {
			*out = append(*out, Change{Op: "removed", Path: append(at("enum"), fmt.Sprint(v)), Before: v})
		}
	}
	da, _ := x["description"].(string)
	db, _ := y["description"].(string)
	if da != db {
		d, stats := unifiedDiff(da, db)
		*out = append(*out, Change{Op: "changed", Path: at("description"), TextDiff: &d, Stats: &stats})
	}
}

// sourceTag is the " at <tag>" a source names its build by.
var sourceTag = regexp.MustCompile(` at \S+$`)

// sourceKind is where options were read from, the build's own tag left out.
// approximate reports options read from minified code, where a nested schema or a type is not
// read the same way from build to build.
func approximate(kind string) bool { return strings.Contains(kind, "minified") }

// withoutType is an option without its type.
func withoutType(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, x := range m {
		if k != "type" {
			out[k] = x
		}
	}
	return out
}

func sourceKind(v any) string {
	s, _ := v.(string)
	return sourceTag.ReplaceAllString(s, "")
}

// envKind is how a config's variables were read: a capture before it said so read strings.
func envKind(cfg map[string]any) string {
	if s, _ := cfg["env_source"].(string); s != "" {
		return s
	}
	return "the names its strings hold"
}

func union(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// guarded are the words of an option's path that say it guards what the harness may do: a
// new one of these is worth knowing of.
var guarded = regexp.MustCompile(`(?i)sandbox|permission|approv|allow|deny|denied|trust|network|proxy|credential|secret|api_?key|polic|security|yolo|bypass|danger|auth`)

// configImpact is a config change's impact.
func configImpact(c *Change) string {
	p := c.Path
	if len(p) < 3 || p[1] != "options" {
		return "informational"
	}
	field := ""
	if len(p) > 3 {
		field = p[3]
	}
	switch {
	case len(p) == 3 && c.Op == "removed":
		return "notable" // a setting no longer read
	case len(p) == 3 && c.Op == "added" && guarded.MatchString(p[2][strings.LastIndex(p[2], ".")+1:]):
		// The option's own key, not its table's: a new table's keys are not each a guard.
		return "notable"
	case field == "default" || field == "type":
		return "notable"
	case field == "enum" && c.Op == "removed":
		return "notable"
	case field == "deprecated" && c.After == true:
		return "notable"
	}
	return "informational"
}

// configTitle is a config change said in a line.
func configTitle(c *Change, arrow func() string, stats string) string {
	p := c.Path
	at := func(i int) string {
		if i < len(p) {
			return p[i]
		}
		return ""
	}
	switch at(1) {
	case "options":
		opt := at(2)
		switch {
		case len(p) == 3 && c.Op == "added":
			return "New setting: " + opt
		case len(p) == 3 && c.Op == "removed":
			return "Setting no longer read: " + opt
		case at(3) == "default":
			return fmt.Sprintf("Default of %s: %s → %s", opt, sayValue(c.Before), sayValue(c.After))
		case at(3) == "type":
			return fmt.Sprintf("Type of %s: %s", opt, arrow())
		case at(3) == "enum" && c.Op == "added":
			return fmt.Sprintf("Setting %s gained value %s", opt, at(4))
		case at(3) == "enum" && c.Op == "removed":
			return fmt.Sprintf("Setting %s lost value %s", opt, at(4))
		case at(3) == "description":
			return opt + " description changed" + stats
		case at(3) == "deprecated" && c.After == true:
			return "Setting " + opt + " deprecated"
		case at(3) == "deprecated":
			return "Setting " + opt + " no longer deprecated"
		}
	case "source":
		return "Configuration options read from: " + arrow()
	case "env_source":
		return "Environment variables read from: " + arrow()
	case "env_vars", "settings_keys":
		if len(c.Path) == 2 && at(1) == "env_vars" {
			added, _ := c.After.([]string)
			removed, _ := c.Before.([]string)
			return fmt.Sprintf("Environment variables: +%d −%d", len(added), len(removed))
		}
		what := map[string]string{"env_vars": "environment variable", "settings_keys": "setting"}[at(1)]
		if len(p) == 3 {
			if c.Op == "added" {
				return "New " + what + ": " + at(2)
			}
			return strings.ToUpper(what[:1]) + what[1:] + " no longer read: " + at(2)
		}
	}
	return ""
}

// sayValue is a default as a title says it: none for no default, "" for an empty string, a
// long one cut short.
func sayValue(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return "none"
	case string:
		if x == "" {
			return `""`
		}
		s = x
	default:
		data, _ := json.Marshal(v)
		s = string(data)
	}
	if len(s) > 60 {
		return s[:57] + "…"
	}
	return s
}

// profileOption is an option a profile repeats: "profiles.<name>.model" is "model" set for one
// profile (Codex's [profiles.x] tables take most top-level keys).
const profileOption = "profiles.<name>."

// collapseProfiles drops a profile's copy of a change its top-level option has the same: a new
// setting is one change, not one and its profile's. A change only a profile has is kept.
func collapseProfiles(changes []Change) []Change {
	top := map[string]bool{}
	sig := func(c Change, key string) string {
		data, _ := json.Marshal([]any{c.Op, key, c.Path[3:], c.Before, c.After})
		return string(data)
	}
	for _, c := range changes {
		if len(c.Path) >= 3 && c.Path[1] == "options" && !strings.HasPrefix(c.Path[2], profileOption) {
			top[sig(c, c.Path[2])] = true
		}
	}
	out := changes[:0]
	for _, c := range changes {
		if len(c.Path) >= 3 && c.Path[1] == "options" {
			if rest, ok := strings.CutPrefix(c.Path[2], profileOption); ok && top[sig(c, rest)] {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// envGroup is how many variables added or removed a diff lists one by one: past it, they are
// one change with their lists, as a wide range of builds (or a heuristic reading) has hundreds.
const envGroup = 20

// groupEnvVars makes many variables added and removed one change: "Environment variables: +309
// −70", the names in its before (removed) and after (added).
func groupEnvVars(changes []Change) []Change {
	var added, removed []string
	n := 0
	for _, c := range changes {
		if len(c.Path) == 3 && c.Path[1] == "env_vars" {
			n++
			switch c.Op {
			case "added":
				added = append(added, c.Path[2])
			case "removed":
				removed = append(removed, c.Path[2])
			}
		}
	}
	if n <= envGroup {
		return changes
	}
	out := changes[:0]
	for _, c := range changes {
		if !(len(c.Path) == 3 && c.Path[1] == "env_vars") {
			out = append(out, c)
		}
	}
	return append(out, Change{Op: "changed", Path: []string{"config", "env_vars"}, Before: nonNilStrings(removed), After: nonNilStrings(added)})
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
