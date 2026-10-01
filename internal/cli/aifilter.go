package cli

import (
	"regexp"
	"slices"
	"strings"
)

// sessionSelector renders the session flags as an AIP-160 filter: values in a field are
// ORed, fields and --filter are ANDed. Principal fields carry ids (see principalIndex).
type sessionSelector struct {
	sources, userIDs, apiKeyIDs, models, providers []string
	extra                                          string
}

func (s sessionSelector) filter() string {
	var groups []string
	add := func(field, op string, values []string) {
		values = dedupe(values)
		if len(values) == 0 {
			return
		}
		terms := make([]string, 0, len(values))
		for _, v := range values {
			terms = append(terms, field+op+aipQuote(v))
		}
		if len(terms) == 1 {
			groups = append(groups, terms[0])
			return
		}
		groups = append(groups, "("+strings.Join(terms, " OR ")+")")
	}
	add("source_system", "=", s.sources)
	add("user_id", "=", s.userIDs)
	add("api_key_id", "=", s.apiKeyIDs)
	// model and provider are membership tests, spelled `:` in AIP-160.
	add("model", ":", s.models)
	add("provider", ":", s.providers)
	if extra := strings.TrimSpace(s.extra); extra != "" {
		if len(groups) == 0 {
			return extra
		}
		groups = append(groups, "("+extra+")")
	}
	return strings.Join(groups, " AND ")
}

// aipQuote escapes only the quote and the backslash; Unicode passes through.
func aipQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// promMatcher is exact for one value and an anchored alternation for several; empty
// when there is nothing to match, so callers can append unconditionally.
func promMatcher(label string, values []string) string {
	values = dedupe(values)
	switch len(values) {
	case 0:
		return ""
	case 1:
		return label + `="` + promQuote(values[0]) + `"`
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = regexp.QuoteMeta(v)
	}
	return label + `=~"` + promQuote(strings.Join(quoted, "|")) + `"`
}

func promQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// dedupe keeps first-seen order so a rendered filter reads in the order the flags were given.
func dedupe(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || slices.Contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}
