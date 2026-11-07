package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
)

// Normalization makes one build's capture read the same whichever machine, night and
// sandbox took it: the run's own literals (its paths, the fixture's URL, the prompt) become
// the placeholders the profile names, and what varies by itself (ids, dates, times, ports,
// the machine's timezone and OS release) becomes a placeholder of its kind. Two snapshots
// then differ only where the builds do.

// patterns are the volatile values, most specific first: each is replaced before the next
// is looked for.
var patterns = []struct {
	re   *regexp.Regexp
	with string
}{
	// A date stands alone: one inside a name (a beta's, a model's) is part of the name.
	{regexp.MustCompile(`(^|[^\w-])\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "${1}<timestamp>"},
	{uuidPattern, uuidWith},
	{regexp.MustCompile(`(^|[^\w-])\d{4}-\d{2}-\d{2}\b`), "${1}<date>"},
	{regexp.MustCompile(`\b(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday),? (?:January|February|March|April|May|June|July|August|September|October|November|December) \d{1,2},? \d{4}\b`), "<date>"},
	{regexp.MustCompile(`\b(?:January|February|March|April|May|June|July|August|September|October|November|December) \d{1,2},? \d{4}\b`), "<date>"},
	{regexp.MustCompile(`(?:/private)?/var/folders/[^\s"'<>)\]]+`), "<tmp>"},
	{regexp.MustCompile(`(?:/private)?/tmp/[^\s"'<>)\]]+`), "<tmp>"},
	{regexp.MustCompile(`\b(?:127\.0\.0\.1|localhost|\[::1\]):\d{2,5}\b`), "<local>"},
	{regexp.MustCompile(`\b(?:Africa|America|Antarctica|Asia|Atlantic|Australia|Europe|Indian|Pacific|Etc)/[A-Z][A-Za-z_]+(?:/[A-Z][A-Za-z_]+)?\b`), "<timezone>"},
	{regexp.MustCompile(`\b(Mac OS|macOS|Darwin|Linux) \d+(?:\.\d+){1,2}\b`), "$1 <os-release>"},
	{regexp.MustCompile(`\b[0-9a-f]{32,64}\b`), "<hex>"},
	// The scratch repository's commits, as a harness lists them in its git context.
	{regexp.MustCompile(`(Recent commits:\s*\n)[0-9a-f]{7,12}\b`), "${1}<commit>"},
}

// uuidPattern is an id that stands alone or after an underscore (Claude Desktop's
// local_<uuid> session ids): \b never falls between _ and a digit, both word characters.
var uuidPattern = regexp.MustCompile(`(?i)(^|[^0-9a-z])[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(_|\b)`)

const uuidWith = "${1}<uuid>${2}"

var (
	nonAlnum    = regexp.MustCompile(`[^A-Za-z0-9]`)
	buildSuffix = regexp.MustCompile(`<version>\.[0-9a-f]{3,12}\b`)
)

// normalize is p with its placeholders and volatile values replaced everywhere: in every
// string, header and key.
func normalize(p e2e.Profile) e2e.Profile {
	literals := make([]string, 0, len(p.Placeholders)*2)
	with := map[string]string{}
	for lit, name := range p.Placeholders {
		if len(lit) < 4 {
			continue
		}
		forms := []string{lit}
		// macOS spells a temporary directory both ways.
		if strings.HasPrefix(lit, "/var/") {
			forms = append(forms, "/private"+lit)
		}
		for _, form := range forms {
			literals = append(literals, form)
			with[form] = name
		}
	}
	// A harness that keeps state by project names the project by its path, every character but
	// letters and digits a dash (Claude Code's projects/<slug>/). A slug the profile named itself
	// (<project>) keeps its name: one slug, one placeholder, whichever map order.
	for _, form := range append([]string(nil), literals...) {
		if !strings.HasPrefix(form, "/") {
			continue
		}
		slug := nonAlnum.ReplaceAllString(form, "-")
		if _, named := with[slug]; named {
			continue
		}
		literals = append(literals, slug)
		with[slug] = strings.TrimSuffix(with[form], ">") + "-slug>"
	}
	// Longest first, so a sandbox's repository is <repo>, not <sandbox>/repo.
	sort.Slice(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })
	data, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var tree any
	if err := json.Unmarshal(data, &tree); err != nil {
		return p
	}
	// The build's own version, where it stands alone (a user agent's, a billing header's),
	// would read as a change in every build.
	var version *regexp.Regexp
	if len(p.Version) >= 5 {
		version = regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(p.Version) + `($|[^\w])`)
	}
	tree = walkStrings(tree, func(s string) string { return normalizeText(s, literals, with, version) })
	data = marshalPlain(tree)
	var out e2e.Profile
	if err := json.Unmarshal(data, &out); err != nil {
		// A profile normalization cannot read back is a bug here, not a build's: say it.
		panic(fmt.Sprintf("normalize %s %s: %v", p.Harness, p.Version, err))
	}
	out.Placeholders = nil
	return out
}

// normalizeText replaces the literals, then the volatile values, in s.
func normalizeText(s string, literals []string, with map[string]string, version *regexp.Regexp) string {
	for _, lit := range literals {
		if strings.Contains(s, lit) {
			s = strings.ReplaceAll(s, lit, with[lit])
		}
	}
	if version != nil {
		s = version.ReplaceAllString(s, "${1}<version>${2}")
		// A build's own hash beside its version (cc_version=2.1.285.96a) changes with it.
		s = buildSuffix.ReplaceAllString(s, "<version>")
	}
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}

// marshalPlain is v as JSON with its <, > and & as they are: placeholders read as written.
func marshalPlain(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// keptKeys hold the capture's own values, not the build's: when a request arrived.
// A signature's certificate fingerprint, serial and dates are facts of the build, not of the
// run: no id or date of this machine's.
var keptKeys = map[string]bool{"at": true, "checked_at": true, "signed_at": true, "not_before": true,
	"not_after": true, "serial": true, "sha256": true}

// walkStrings applies f to every string in v, map keys included, but for the values of
// keptKeys.
func walkStrings(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for i := range x {
			x[i] = walkStrings(x[i], f)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if keptKeys[k] {
				out[k] = val
				continue
			}
			out[f(k)] = walkStrings(val, f)
		}
		return out
	}
	return v
}

// Machine paths. What a build ran and touched names the machine it ran on (an installed
// program's path, an Xcode's, a user's home, the runner's checkout): e2e.MachinePaths names each
// as on any machine. The recorders apply it as they record; publishing applies it again to
// every snapshot it makes and to every one it loads back, so a rule added there corrects what
// was published before it. It touches observed values alone, never a vendor's own text: a
// system prompt's /home/user/monorepo or a tool's description's /Users/name/My Documents is
// the build's, not the machine's.

// machineField is a part of a snapshot that holds observed values: where the machine's paths
// show, if anywhere.
type machineField struct {
	name string
	get  func(s *Snapshot) any
	set  func(s *Snapshot, v any)
}

// machineFields are the observed parts of a snapshot: the capture's command line, how a desktop
// app launched its agent (its arguments and variables), the hooks' payloads and variables, the
// network's egress, storage's paths, the files profile's paths and commands, and the binary's
// facts. The rest (prompt, tools, help, configuration options, the vendor's docs, live) is the
// vendor's text, or terma's.
var machineFields = []machineField{
	{"capture.command", func(s *Snapshot) any { return s.Capture.Command }, func(s *Snapshot, v any) {
		s.Capture.Command = nil
		for _, a := range v.([]any) {
			s.Capture.Command = append(s.Capture.Command, a.(string))
		}
	}},
	{"cli.launch", func(s *Snapshot) any { return s.CLI["launch"] }, func(s *Snapshot, v any) { s.CLI["launch"] = v }},
	{"hooks", func(s *Snapshot) any { return s.Hooks }, func(s *Snapshot, v any) { s.Hooks = v.(map[string]any) }},
	{"network", func(s *Snapshot) any { return s.Network }, func(s *Snapshot, v any) { s.Network = v.(map[string]any) }},
	{"storage", func(s *Snapshot) any { return s.Storage }, func(s *Snapshot, v any) { s.Storage = v.(map[string]any) }},
	{"filesystem", func(s *Snapshot) any { return s.Filesystem }, func(s *Snapshot, v any) { s.Filesystem = v.(map[string]any) }},
	{"binary", func(s *Snapshot) any { return s.Binary }, func(s *Snapshot, v any) { s.Binary = v.(map[string]any) }},
}

// scrubMachine names the machine's paths in s's observed values (machineFields), and reports
// whether it changed any. A field it leaves as it was keeps its values as they were.
func scrubMachine(s *Snapshot) bool {
	changed := false
	for _, f := range machineFields {
		v := f.get(s)
		if v == nil || reflect.ValueOf(v).IsNil() {
			continue
		}
		before := jsonOf(v)
		after := scrubMachineTree(jsonOf(v))
		if reflect.DeepEqual(before, after) {
			continue
		}
		f.set(s, after)
		changed = true
	}
	return changed
}

// scrubMachineTree is v with the machine's paths named in every string, map keys included. Two
// keys that name the same once named (/opt/homebrew/bin/git status and git status) are one: their
// values merged (mergeObserved), in the order of their keys as they were.
func scrubMachineTree(v any) any {
	switch x := v.(type) {
	case string:
		return scrubMachineText(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = scrubMachineTree(x[i])
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			nk, nv := scrubMachineText(k), scrubMachineTree(x[k])
			if prev, ok := out[nk]; ok {
				nv = mergeObserved(nk, prev, nv)
			}
			out[nk] = nv
		}
		return out
	}
	return v
}

// scrubMachineText is one observed string with the machine's paths named, and a run's id that
// normalization once missed (one after an underscore) a placeholder.
func scrubMachineText(s string) string {
	return uuidPattern.ReplaceAllString(e2e.MachinePaths(s), uuidWith)
}

// mergeObserved is what two observed values become when their keys name the same thing (a
// command run from two installed paths): objects merged key by key, lists the first's with what
// the second adds, counts added, a role either had both, anything else the first's.
func mergeObserved(key string, a, b any) any {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return a
		}
		keys := make([]string, 0, len(y))
		for k := range y {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if prev, ok := x[k]; ok {
				x[k] = mergeObserved(k, prev, y[k])
			} else {
				x[k] = y[k]
			}
		}
		return x
	case []any:
		y, _ := b.([]any)
		for _, item := range y {
			if !slices.ContainsFunc(x, func(have any) bool { return reflect.DeepEqual(have, item) }) {
				x = append(x, item)
			}
		}
		return x
	case float64:
		if y, ok := b.(float64); ok && key == "count" {
			return x + y
		}
	case string:
		if y, ok := b.(string); ok && key == "role" && x != y {
			return e2e.RoleBoth
		}
	}
	return a
}

// machineLeaks are the machine paths each observed value of s still holds once named, by field:
// none, where publishing works. The publish-time guard and the check of published data use it.
func machineLeaks(s *Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, f := range machineFields {
		v := f.get(s)
		if v == nil || reflect.ValueOf(v).IsNil() {
			continue
		}
		walkStrings(jsonOf(v), func(str string) string {
			if l := e2e.MachineLeak(str); l != "" {
				out[f.name] = append(out[f.name], str)
			}
			return str
		})
	}
	return out
}
