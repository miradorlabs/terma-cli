// Package globalmode is what `terma setup` writes on a machine in every mode: each agent's
// machine-wide hooks, and the managed configuration an administrator deploys instead. The
// team policy's repository list decides what they record. The commit hooks are not here:
// they go into each repository an agent works in (internal/repohooks).
package globalmode

import (
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Machine is this machine as setup writes it.
type Machine struct {
	// ConfigDir is terma's config directory, where the hooks keep their state.
	ConfigDir string
	Agents    *agents.Registry
	// Terma is the executable the hooks call, by absolute path.
	Terma func() (string, error)
	// ManagedRoot is the root managed configuration is deployed under ("/" on a machine).
	ManagedRoot string
}

// Apply installs the selected agents' machine-wide hooks; said reports each change,
// approved each approval written into an agent's own config (one the developer should
// always see), then each step left to the developer.
func (m Machine) Apply(selected []string, said, approved, then func(string)) error {
	return m.apply(selected, true, said, approved, then)
}

// Remove takes them all out again.
func (m Machine) Remove(said func(string)) error {
	return m.apply(nil, false, said, said, func(string) {})
}

func (m Machine) apply(selected []string, install bool, said, approved, then func(string)) error {
	files, err := m.ApplyUserHooks(selected, install)
	if err != nil {
		return err
	}
	for _, f := range files {
		said("Machine-wide hooks updated: " + output.TildePath(f))
	}
	m.syncUserHookTrust(approved, then)
	for _, step := range m.TrustSteps(selected) {
		then(step)
	}
	return nil
}
