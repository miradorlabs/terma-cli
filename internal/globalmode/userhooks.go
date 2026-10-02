package globalmode

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// In global mode `terma setup` writes machine-wide hooks into each agent's user-level
// hooks file, and a repository's committed hooks step aside for them (hookYields): a
// repository's hooks may not run until each developer trusts them.

const userHooksFile = "user-hooks.json"

type userHooksRecord struct {
	Agents []string `json:"agents"`
}

// TrustSteps are what the developer must do before machine-wide hooks run: one per agent
// that does not yet run all of terma's entries as written. A file setup left unchanged can
// still be one, trusted before an earlier setup rewrote it.
func (m Machine) TrustSteps(selected []string) []string {
	var steps []string
	for _, name := range m.userHookAgents(selected) {
		a, ok := m.Agents.Find[agents.UserHooksTrust](name)
		if !ok || m.ManagedDeployed(name) {
			continue
		}
		// Unreadable trust is not trusted: the step is the way to find out.
		if present, trusted, err := a.UserHooksTrusted(); err != nil || present && !trusted {
			steps = append(steps, a.UserHooksTrustStep())
		}
	}
	return steps
}

func (m Machine) userHookAgents(selected []string) []string {
	var out []string
	for _, a := range m.Agents.With[agents.UserHooks]() {
		for _, choice := range agents.Selections(a) {
			if slices.Contains(selected, choice) {
				out = append(out, a.Name())
				break
			}
		}
	}
	return out
}

// ApplyUserHooks writes terma's entries into each covered agent's machine-wide hooks file,
// or removes them, and records which agents they cover.
func (m Machine) ApplyUserHooks(selected []string, install bool) ([]string, error) {
	terma, err := m.Terma()
	if err != nil {
		return nil, err
	}
	covered := m.userHookAgents(selected)
	var changed []string
	for _, a := range m.Agents.With[agents.UserHooks]() {
		// Not where the organization's managed hooks run: both would fire.
		want := install && slices.Contains(covered, a.Name()) && !m.ManagedDeployed(a.Name())
		path, err := a.UserHooksPath()
		dir, file := filepath.Dir(path), filepath.Base(path)
		if err != nil {
			return changed, err
		}
		if !want {
			if _, err := os.Stat(filepath.Join(dir, file)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(terma), want)
		if err != nil {
			return changed, err
		}
		if plan.Empty() {
			continue
		}
		if want {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return changed, err
			}
		}
		if err := hookmgr.Apply(dir, plan); err != nil {
			return changed, err
		}
		changed = append(changed, filepath.Join(dir, file))
	}
	rec, err := m.userHooksRecordPath()
	if err != nil {
		return changed, err
	}
	if !install {
		if err := os.Remove(rec); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, err
		}
		return changed, nil
	}
	// Recorded whichever hooks run, so a repository's committed hooks step aside either way.
	return changed, config.WriteJSON(rec, userHooksRecord{Agents: covered}, 0o600)
}

func (m Machine) userHooksRecordPath() (string, error) {
	dir, err := m.RelayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, userHooksFile), nil
}

func (m Machine) userHooksCover(tool string) bool {
	path, err := m.userHooksRecordPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var rec userHooksRecord
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	return slices.Contains(rec.Agents, m.Agents.NameForTool(tool))
}

// Yields is true for a leftover machine-wide hook outside global mode, or a committed one
// in global mode whose agent has machine-wide hooks.
func (m Machine) Yields(user bool, pol config.Policy, tool string) bool {
	if user {
		return !pol.Global()
	}
	return pol.Global() && m.userHooksCover(tool)
}

func (m Machine) managedHookFiles(agent string) []string {
	a, ok := m.Agents.Lookup(agent)
	if !ok {
		return nil
	}
	managed, ok := a.(agents.ManagedHooks)
	if !ok {
		return nil
	}
	return managed.ManagedHookFiles(m.ManagedRoot)
}

// ManagedDeployed reports whether the organization's managed hooks run agent's sessions
// here.
func (m Machine) ManagedDeployed(agent string) bool {
	for _, f := range m.managedHookFiles(agent) {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), " hook --user ") {
			return true
		}
	}
	return false
}
