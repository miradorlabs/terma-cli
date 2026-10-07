package config

import (
	"slices"
	"strings"
)

// Repository is how admission names a working copy: its origin remote as `host/path`
// (gitx.RepositoryID), "" outside git or without a hosted origin.
type Repository struct {
	Origin string `json:"origin,omitempty"`
}

// Admits reports whether p collects the sessions and commits of r: every one in global
// mode, else those in a repository its list names.
//
// Each entry is a repository as `host/owner/name`, e.g. `github.com/miradorlabs/mirador-platform`.
// The CLI admits a session only inside a git working copy whose `origin` remote, normalised,
// equals an entry ignoring case: host lowercased without port or credentials, path with
// `.git` and any trailing slash stripped, from scp (`git@host:path`), `https://`, `ssh://`
// and `git://` forms. A linked worktree reads `origin` from its main repository. A folder
// outside git, a repository with no `origin`, or an `origin` that is a local path or
// `file://` URL is never admitted. An entry has a host and at least two path segments, none
// empty; a longer path matches a longer `origin` path exactly (GitLab subgroups). An empty
// list admits nothing.
func (p Policy) Admits(r Repository) bool {
	if p.Global() {
		return true
	}
	return r.Origin != "" && slices.ContainsFunc(p.Repositories, func(e string) bool {
		return validEntry(e) && strings.EqualFold(strings.TrimSpace(e), r.Origin)
	})
}

// AdmitsNone reports whether p admits no repository at all: not validated, or repository
// mode with no usable entry.
func (p Policy) AdmitsNone() bool {
	if !p.Validated() {
		return true
	}
	return !p.Global() && !slices.ContainsFunc(p.Repositories, validEntry)
}

// validEntry is a host and at least two path segments, none empty.
func validEntry(e string) bool {
	parts := strings.Split(strings.TrimSpace(e), "/")
	return len(parts) >= 3 && !slices.Contains(parts, "")
}

// Policies are the policies one machine collects under, in the order they are asked
// (routing.Collection builds them): Admitting takes the first that admits a repository.
type Policies []Policy

// Admitting is the policy that collects r: the first whose repository list names it, else
// the first global one, which collects every session. Repository mode is asked first across
// every policy, so a repository a team lists goes to that team even while another team
// collects everything on the machine.
func (ps Policies) Admitting(r Repository) (Policy, bool) {
	for _, p := range ps {
		if !p.Global() && p.Admits(r) {
			return p, true
		}
	}
	return ps.Global()
}

// Global is the global policy among ps, which collects every session the repository-mode
// ones do not place; false when none is.
func (ps Policies) Global() (Policy, bool) {
	for _, p := range ps {
		if p.Global() {
			return p, true
		}
	}
	return Policy{}, false
}

// Validated reports whether any of ps is a fetched policy.
func (ps Policies) Validated() bool {
	return slices.ContainsFunc(ps, Policy.Validated)
}
