package config

import (
	"slices"
	"strings"
)

// Repository is how a working copy is named for admission: the folder names and origin's
// repository name a folder entry may equal, and origin's owner/name.
type Repository struct {
	Names []string `json:"names,omitempty"`
	Path  string   `json:"path,omitempty"`
}

// Admits reports whether p collects the sessions and commits of r: every one in global
// mode, else those its folder list names. An entry with a slash matches origin's
// owner/name; one without equals any of r's names, ignoring case: the git root's folder
// name (for a linked worktree, also its main checkout's folder name) or the origin
// remote's repository name.
func (p Policy) Admits(r Repository) bool {
	if p.Global() {
		return true
	}
	for _, entry := range p.Folders {
		entry = strings.Trim(strings.TrimSpace(entry), "/")
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if strings.EqualFold(entry, r.Path) {
				return true
			}
		} else if slices.ContainsFunc(r.Names, func(n string) bool { return strings.EqualFold(entry, n) }) {
			return true
		}
	}
	return false
}

// AdmitsNone reports whether p admits no folder at all: not validated, or repository mode
// with an empty list.
func (p Policy) AdmitsNone() bool {
	if !p.Validated() {
		return true
	}
	return !p.Global() && !slices.ContainsFunc(p.Folders, func(f string) bool { return strings.Trim(strings.TrimSpace(f), "/") != "" })
}

// Equal reports whether r and o name the same working copy.
func (r Repository) Equal(o Repository) bool {
	return r.Path == o.Path && slices.Equal(r.Names, o.Names)
}
