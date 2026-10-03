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
