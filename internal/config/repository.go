package config

import "strings"

// Repository is how a working copy is named for admission: the last segment of its
// remote's path and the whole path (owner/name), else only its folder's name.
type Repository struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

// Admits reports whether p collects the sessions and commits of r: every one in global
// mode, else those its repository list names. An entry with a slash matches the remote's
// owner/name, one without the repository's name, ignoring case.
func (p Policy) Admits(r Repository) bool {
	if p.Global() {
		return true
	}
	for _, entry := range p.Repositories {
		entry = strings.Trim(strings.TrimSpace(entry), "/")
		target := r.Name
		if strings.Contains(entry, "/") {
			target = r.Path
		}
		if target != "" && strings.EqualFold(entry, target) {
			return true
		}
	}
	return false
}
