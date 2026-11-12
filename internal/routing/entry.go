package routing

import (
	"slices"
	"strings"
)

// Normalize is the one form binding keys, policy entries and repositories are compared
// in: trimmed, without a trailing slash, compared ignoring case as config.Policy.Admits
// compares them. A repository reaches it already as gitx.RepositoryID makes it, host
// lowercased without port or credentials.
func Normalize(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}

// Owner is a repository's host and first path segment, `github.com/acme` for
// `github.com/acme/api` and `gitlab.com/acme` for `gitlab.com/acme/platform/api`: the key
// an owner binding is kept under. "" when repo has no owner and name.
func Owner(repo string) string {
	parts := strings.Split(Normalize(repo), "/")
	if len(parts) < 3 || slices.Contains(parts, "") {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// isOwnerKey reports whether a normalised binding key names an owner, `host/owner`,
// rather than a repository, which has at least a host, an owner and a name.
func isOwnerKey(key string) bool {
	return strings.Count(key, "/") == 1
}

// Entry is one repository entry of a team's policy: an exact repository, or a wildcard
// over every repository below a prefix of at least a host and an owner.
type Entry struct {
	// Raw is the entry as the policy lists it.
	Raw      string
	prefix   []string
	wildcard bool
}

// ParseEntry reads raw as a policy lists it. An entry is a host and at least two path
// segments, none empty. With wildcards, it may instead end in a whole `/*` segment after
// at least a host and an owner; without, `/*` is an ordinary last segment that no origin
// has, as an older reader takes it. `github.com/*`, `*` and `github.com/acme/terma-*`
// are never wildcards. False for an entry that is neither.
func ParseEntry(raw string, wildcards bool) (Entry, bool) {
	parts := strings.Split(Normalize(raw), "/")
	if slices.Contains(parts, "") {
		return Entry{}, false
	}
	if wildcards && parts[len(parts)-1] == "*" {
		prefix := parts[:len(parts)-1]
		if len(prefix) < 2 || slices.ContainsFunc(prefix, func(p string) bool { return strings.Contains(p, "*") }) {
			return Entry{}, false
		}
		return Entry{Raw: raw, prefix: prefix, wildcard: true}, true
	}
	if len(parts) < 3 || wildcards && slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(p, "*") }) {
		return Entry{}, false
	}
	return Entry{Raw: raw, prefix: parts}, true
}

// Matches reports whether the entry names repo: an exact entry equals it, a wildcard is a
// strict prefix of it by whole segments.
func (e Entry) Matches(repo string) bool {
	parts := strings.Split(Normalize(repo), "/")
	if len(parts) < 3 || slices.Contains(parts, "") {
		return false
	}
	if !e.wildcard {
		return slices.Equal(parts, e.prefix)
	}
	return len(parts) > len(e.prefix) && slices.Equal(parts[:len(e.prefix)], e.prefix)
}

// Wildcard reports whether the entry covers every repository below its prefix.
func (e Entry) Wildcard() bool { return e.wildcard }

// Key is the binding the entry needs before it may collect: its owner for a wildcard,
// its repository for an exact entry, which its owner's binding also satisfies.
func (e Entry) Key() string {
	if e.wildcard {
		return e.prefix[0] + "/" + e.prefix[1]
	}
	return strings.Join(e.prefix, "/")
}

// moreSpecific reports whether a is to be tried before b: an exact entry before any
// wildcard, and between wildcards, the one with more fixed segments.
func moreSpecific(a, b Entry) bool {
	if a.wildcard != b.wildcard {
		return !a.wildcard
	}
	return len(a.prefix) > len(b.prefix)
}

// bestEntry is the most specific of entries that matches repo, read under wildcards.
func bestEntry(entries []string, wildcards bool, repo string) (Entry, bool) {
	var best Entry
	found := false
	for _, raw := range entries {
		e, ok := ParseEntry(raw, wildcards)
		if !ok || !e.Matches(repo) {
			continue
		}
		if !found || moreSpecific(e, best) {
			best, found = e, true
		}
	}
	return best, found
}
