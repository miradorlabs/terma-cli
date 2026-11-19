package routing

import "github.com/miradorlabs/terma-cli/internal/config"

// The policy-entry grammar is config's (config/entry.go), so config.Policy.Admits reads
// an entry exactly as routing does; these are routing's names for it.

// Entry is one repository entry of a team's policy (config.Entry).
type Entry = config.Entry

// ParseEntry reads raw as a policy lists it (config.ParseEntry).
func ParseEntry(raw string, wildcards bool) (Entry, bool) { return config.ParseEntry(raw, wildcards) }

// Normalize is the one form keys, entries and repositories are compared in (config.Normalize).
func Normalize(s string) string { return config.Normalize(s) }

// Owner is a repository's host and first path segment (config.Owner).
func Owner(repo string) string { return config.Owner(repo) }

// moreSpecific reports whether a is to be tried before b: an exact entry before any
// wildcard, and between wildcards, the one with more fixed segments.
func moreSpecific(a, b Entry) bool {
	if a.Wildcard() != b.Wildcard() {
		return !a.Wildcard()
	}
	return a.Fixed() > b.Fixed()
}
