package globalmode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// `terma setup` writes machine-wide hooks into each agent's user-level hooks file.

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

// syncUserHookTrust keeps each agent's approvals of terma's machine-wide entries in step
// with the hooks file as ApplyUserHooks left it: approved while there, withdrawn once gone.
// A failure leaves TrustSteps to say what the developer does instead.
func (m Machine) syncUserHookTrust(approved, then func(string)) {
	terma, err := m.Terma()
	if err != nil {
		return
	}
	for _, a := range m.Agents.With[agents.UserHooks]() {
		t, ok := a.(agents.HookTrusting)
		if !ok {
			continue
		}
		path, err := a.UserHooksPath()
		if err != nil {
			continue
		}
		done, err := t.SyncHookTrust(path, hookmgr.UserHookCommand(terma))
		switch {
		case err != nil:
			then("Terma could not approve its " + a.DisplayName() + " hooks itself (" + err.Error() + ").")
		case done.Approved > 0:
			approved(fmt.Sprintf("%s: approved Terma's %d machine-wide hooks (review them with /hooks)", a.DisplayName(), done.Approved))
		}
	}
}

// ApplyUserHooks writes terma's entries into each covered agent's machine-wide hooks file,
// or removes them, and returns the files it changed.
func (m Machine) ApplyUserHooks(selected []string, install bool) ([]string, error) {
	changed, _, err := m.applyUserHooks(selected, install)
	return changed, err
}

// applyUserHooks is ApplyUserHooks, with the notes each agent's plan carries, prefixed with
// its name. An agent this build does not offer on this system is left alone: a home
// directory shared with another system keeps that system's hooks.
func (m Machine) applyUserHooks(selected []string, install bool) (changed, notes []string, _ error) {
	terma, err := m.Terma()
	if err != nil {
		return nil, nil, err
	}
	covered := m.userHookAgents(selected)
	for _, a := range m.Agents.With[agents.UserHooks]() {
		if !m.Agents.IsSupported(a.Name()) {
			continue
		}
		// Not where the organization's managed hooks run: both would fire.
		want := install && slices.Contains(covered, a.Name()) && !m.ManagedDeployed(a.Name())
		paths, err := agents.UserHooksPaths(a)
		if err != nil {
			return changed, notes, err
		}
		for _, path := range paths {
			done, err := m.applyAgentHooks(a, path, terma, want, &notes)
			if err != nil {
				return changed, notes, err
			}
			if done {
				changed = append(changed, path)
			}
		}
	}
	return changed, notes, nil
}

// applyAgentHooks writes or removes one agent's entries in its hooks file at path,
// reporting whether the file changed.
func (m Machine) applyAgentHooks(a agents.UserHooks, path, terma string, want bool, notes *[]string) (bool, error) {
	dir := filepath.Dir(path)
	if !want {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(terma), want)
	if err != nil {
		return false, err
	}
	// A note can come with no change: one that says why a file was left out.
	for _, note := range plan.Notes {
		*notes = append(*notes, a.DisplayName()+": "+note)
	}
	if plan.Empty() {
		return false, nil
	}
	if want {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
	}
	// Often a dotfiles link: written through, as the exporter step does.
	if err := hookmgr.ApplyThroughLinks(dir, plan); err != nil {
		return false, err
	}
	return true, nil
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
