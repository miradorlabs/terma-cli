package cmd

import (
	"regexp"
	"slices"
	"strings"
)

// sessionSelector is what the --source/--user/--api-key/--model/--provider flags
// select, rendered to the AIP-160 filter the session endpoints take: values within a
// field are ORed, fields are ANDed, and a hand-written --filter expression is ANDed
// on. Principal fields carry ids — names are resolved before this point (see
// principalIndex).
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
	// model and provider are membership tests — the session used this one — spelled
	// `:` in AIP-160.
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

// aipQuote renders an AIP-160 string literal. Only the quote and the backslash need
// escaping; everything else, including Unicode, passes through.
func aipQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// promMatcher renders one PromQL label matcher: exact for a single value, an
// alternation regex (which PromQL anchors on both ends) for several. Empty when
// there is nothing to match, so callers can append unconditionally.
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

// promQuote escapes a PromQL double-quoted string literal.
func promQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// dedupe drops blank and repeated values, keeping first-seen order so a rendered
// filter reads in the order the flags were given.
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
