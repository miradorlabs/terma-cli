// Package globalmode is what `terma setup` writes on a machine in every mode: each agent's
// machine-wide hooks, git's global core.hooksPath, and the managed configuration an
// administrator deploys instead. The team policy's folder list decides what they record.
package globalmode

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Machine is this machine as global mode writes it.
type Machine struct {
	Agents *agents.Registry
	// Terma is the executable the hooks call, by absolute path.
	Terma func() (string, error)
	// ManagedRoot is the root managed configuration is deployed under ("/" on a machine).
	ManagedRoot string
}

// Apply installs the selected agents' machine-wide hooks and git's global hooks path;
// said reports each change, approved each approval written into an agent's own config
// (one the developer should always see), then each step left to the developer.
func (m Machine) Apply(ctx context.Context, selected []string, said, approved, then func(string)) error {
	return m.apply(ctx, selected, true, said, approved, then)
}

// Remove takes them all out again, restoring git's previous global hooks path.
func (m Machine) Remove(ctx context.Context, said func(string)) error {
	return m.apply(ctx, nil, false, said, said, func(string) {})
}

func (m Machine) apply(ctx context.Context, selected []string, install bool, said, approved, then func(string)) error {
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
	if !install {
		if err := unwireClones(ctx); err != nil {
			return fmt.Errorf("clones' hooks: %w", err)
		}
	}
	changed, err := m.ApplyGitHooks(ctx, install)
	if err != nil {
		return fmt.Errorf("git's global hooks: %w", err)
	}
	if changed && install {
		said("Git: commits in the team's folders are stamped (git config --global core.hooksPath); each repository's own hooks still run")
	} else if changed {
		said("Git: global hooks path restored")
	}
	return nil
}
