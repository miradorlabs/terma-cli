package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
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
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>"},
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
