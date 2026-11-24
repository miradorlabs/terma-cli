package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
)

const teamNamesDir = "team-names"

// teamNames caches the organization and team names `terma teams` last listed per profile.
type teamNames struct {
	Teams   map[string]string `json:"teams"`
	Orgs    map[string]string `json:"organizations"`
	changed bool
}

func teamNamesPath(stateDir, profile string) string {
	return filepath.Join(stateDir, teamNamesDir, profile+".json")
}

func loadTeamNames(stateDir, profile string) teamNames {
	n := teamNames{Teams: map[string]string{}, Orgs: map[string]string{}}
	if stateDir == "" || profile == "" || profile != filepath.Base(profile) {
		return n
	}
	if b, err := os.ReadFile(teamNamesPath(stateDir, profile)); err == nil {
		_ = json.Unmarshal(b, &n)
	}
	if n.Teams == nil {
		n.Teams = map[string]string{}
	}
	if n.Orgs == nil {
		n.Orgs = map[string]string{}
	}
	return n
}

func (n *teamNames) team(id, name string) { n.set(n.Teams, id, name) }
func (n *teamNames) org(id, name string)  { n.set(n.Orgs, id, name) }

func (n *teamNames) set(m map[string]string, id, name string) {
	if id != "" && name != "" && m[id] != name {
		m[id] = name
		n.changed = true
	}
}

func (n teamNames) save(stateDir, profile string) {
	if !n.changed || stateDir == "" || profile == "" || profile != filepath.Base(profile) {
		return
	}
	path := teamNamesPath(stateDir, profile)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if b, err := json.MarshalIndent(n, "", "  "); err == nil {
		_ = config.WriteFileAtomic(path, b, 0o600)
	}
}
