package routing

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// Normalize is the one form binding keys, policy entries and repositories are compared
// in: lowercased whole, as config.Policy.Admits compares ignoring case, then
// gitx.RepositoryID's `host/path`, without scheme, port, credentials, `.git` or a
// trailing slash, from scp, `https://`, `ssh://` and bare `host/path` forms alike. It is
// idempotent. "" for what names no hosted repository, including anything still
// percent-encoded, which no two readers would decode alike.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	id := gitx.RepositoryID(s)
	if id == "" && !strings.Contains(s, "://") {
		id = gitx.RepositoryID("https://" + s)
	}
	if strings.Contains(id, "%") {
		return ""
	}
	return id
}

// segments counts the path segments of a normalised key, host included; 0 if one is
// empty, `.` or `..`.
func segments(key string) int {
	if key == "" {
		return 0
	}
	parts := strings.Split(key, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return 0
		}
	}
	return len(parts)
}

// emptySegment reports whether raw, as typed, has an empty path segment, which
// Normalize would fold away: an entry or a binding key with one is refused, not
// reinterpreted.
func emptySegment(raw string) bool {
	if _, rest, ok := strings.Cut(raw, "://"); ok {
		raw = rest
	}
	return strings.Contains(raw, "//")
}

// validRepo reports whether a normalised key names a repository: a host, an owner and a
// name at least.
func validRepo(key string) bool { return segments(key) >= 3 && !strings.Contains(key, "*") }

// isOwnerKey reports whether a normalised key names an owner, `host/owner`.
func isOwnerKey(key string) bool { return segments(key) == 2 && !strings.Contains(key, "*") }

// owner is a normalised repository's host and first path segment.
func owner(repo string) string {
	host, rest, _ := strings.Cut(repo, "/")
	name, _, _ := strings.Cut(rest, "/")
	return host + "/" + name
}

// Owner is a repository's host and first path segment, `github.com/acme` for
// `github.com/acme/api` and `gitlab.com/acme` for `gitlab.com/acme/platform/api`: the key
// an owner binding is kept under. "" when repo names no repository. Unlike
// gitx.ParseOrigin's owner, it never includes a GitLab subgroup.
func Owner(repo string) string {
	repo = Normalize(repo)
	if !validRepo(repo) {
		return ""
	}
	return owner(repo)
}

// Entry is one repository entry of a team's policy: an exact repository, or a wildcard
// over every repository below a prefix of at least a host and an owner.
type Entry struct {
	// Raw is the entry as the policy lists it.
	Raw string
	// key is the normalised repository, or a wildcard's normalised prefix.
	key      string
	fixed    int
	wildcard bool
}

// ParseEntry reads raw as a policy lists it. An entry is a host and at least two path
// segments, none empty. With wildcards, it may instead end in a whole `/*` segment after
// at least a host and an owner; without, `/*` is an ordinary last segment that no origin
// has, as an older reader takes it. `github.com/*`, `*` and `github.com/acme/terma-*`
// are never wildcards. False for an entry that is neither.
func ParseEntry(raw string, wildcards bool) (Entry, bool) {
	if emptySegment(raw) {
		return Entry{}, false
	}
	norm := Normalize(raw)
	if prefix, ok := strings.CutSuffix(norm, "/*"); wildcards && ok {
		if n := segments(prefix); n >= 2 && !strings.Contains(prefix, "*") {
			return Entry{Raw: raw, key: prefix, fixed: n, wildcard: true}, true
		}
		return Entry{}, false
	}
	n := segments(norm)
	if n < 3 || wildcards && strings.Contains(norm, "*") {
		return Entry{}, false
	}
	return Entry{Raw: raw, key: norm, fixed: n}, true
}

// Matches reports whether the entry names repo: an exact entry equals it, a wildcard is a
// strict prefix of it by whole segments.
func (e Entry) Matches(repo string) bool {
	repo = Normalize(repo)
	return validRepo(repo) && e.matches(repo)
}

// matches is Matches for a repository already normalised.
func (e Entry) matches(repo string) bool {
	if !e.wildcard {
		return repo == e.key
	}
	return strings.HasPrefix(repo, e.key+"/")
}

// Wildcard reports whether the entry covers every repository below its prefix.
func (e Entry) Wildcard() bool { return e.wildcard }

// Key is the binding the entry needs before it may collect: its owner for a wildcard,
// its repository for an exact entry, which its owner's binding also satisfies.
func (e Entry) Key() string {
	if e.wildcard {
		return owner(e.key)
	}
	return e.key
}

// moreSpecific reports whether a is to be tried before b: an exact entry before any
// wildcard, and between wildcards, the one with more fixed segments.
func moreSpecific(a, b Entry) bool {
	if a.wildcard != b.wildcard {
		return !a.wildcard
	}
	return a.fixed > b.fixed
}
