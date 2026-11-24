package cli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
)

type teamNames struct {
	Teams   map[string]string `json:"teams"`
	Orgs    map[string]string `json:"organizations"`
	changed bool
}

func teamNamesPath(stateDir, profile string) (string, bool) {
	if stateDir == "" || profile == "" || profile != filepath.Base(profile) {
		return "", false
	}
	return filepath.Join(stateDir, "team-names", profile+".json"), true
}

func loadTeamNames(stateDir, profile string) teamNames {
	n := teamNames{Teams: map[string]string{}, Orgs: map[string]string{}}
	if path, ok := teamNamesPath(stateDir, profile); ok {
		var raw teamNames
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &raw) == nil {
			maps.Copy(n.Teams, raw.Teams)
			maps.Copy(n.Orgs, raw.Orgs)
		}
	}
	return n
}

func (n *teamNames) set(m map[string]string, id, name string) {
	if id != "" && name != "" && m[id] != name {
		m[id], n.changed = name, true
	}
}
func (n *teamNames) team(id, name string) { n.set(n.Teams, id, name) }
func (n *teamNames) org(id, name string)  { n.set(n.Orgs, id, name) }
func (n teamNames) save(stateDir, profile string) {
	if path, ok := teamNamesPath(stateDir, profile); n.changed && ok && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		if b, err := json.MarshalIndent(n, "", "  "); err == nil {
			_ = config.WriteFileAtomic(path, b, 0o600)
		}
	}
}
