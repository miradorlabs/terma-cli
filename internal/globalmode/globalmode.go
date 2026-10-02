// Package globalmode is what an organization's global mode writes on a machine: each
// agent's machine-wide hooks, git's global core.hooksPath, and the managed configuration
// an administrator deploys instead. Invasive on purpose, and never the default.
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
	// RelayDir is the relay's state directory, which records the agents global mode covers.
	RelayDir func() (string, error)
}

// Apply installs global mode's machine-wide agent hooks and git's global hooks path, or
// removes them; said reports each change, approved each approval written into an agent's
// own config (one the developer should always see), then each step left to the developer.
func (m Machine) Apply(ctx context.Context, selected []string, global bool, said, approved, then func(string)) error {
	files, err := m.ApplyUserHooks(selected, global)
	if err != nil {
		return err
	}
	for _, f := range files {
		said("Machine-wide hooks updated: " + output.TildePath(f))
	}
	m.syncUserHookTrust(approved, then)
	if global {
		for _, step := range m.TrustSteps(selected) {
			then(step)
		}
	}
	changed, err := m.ApplyGitHooks(ctx, global)
	if err != nil {
		return fmt.Errorf("git's global hooks: %w", err)
	}
	if changed && global {
		said("Git: every repository's commits are stamped (git config --global core.hooksPath); each repository's own hooks still run")
	} else if changed {
		said("Git: global hooks path restored")
	}
	return nil
}
