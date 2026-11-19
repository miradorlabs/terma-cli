package config

import "slices"

// Repository is how admission names a working copy: its origin remote as `host/path`
// (gitx.RepositoryID), "" outside git or without a hosted origin.
type Repository struct {
	Origin string `json:"origin,omitempty"`
}

// Admits reports whether p collects the sessions and commits of r: every one in global
// mode, else those in a repository an entry of its list names, read by ParseEntry.
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
//
// On a policy whose major allows them (Wildcards, 2.x), an entry ending in `/*` after a
// host and an owner admits every repository under that prefix: `github.com/acme/*` every
// repository of acme on github.com. On a 1.x policy such an entry is a literal repository,
// which no origin names.
func (p Policy) Admits(r Repository) bool {
	if p.Global() {
		return true
	}
	repo := Normalize(r.Origin)
	return IsRepositoryKey(repo) && slices.ContainsFunc(p.Repositories, func(raw string) bool {
		e, ok := ParseEntry(raw, p.Wildcards)
		return ok && e.matches(repo)
	})
}

// AdmitsNone reports whether p admits no repository at all: not validated, or repository
// mode with no entry ParseEntry reads.
func (p Policy) AdmitsNone() bool {
	if !p.Validated() {
		return true
	}
	return !p.Global() && !slices.ContainsFunc(p.Repositories, func(raw string) bool {
		_, ok := ParseEntry(raw, p.Wildcards)
		return ok
	})
}
