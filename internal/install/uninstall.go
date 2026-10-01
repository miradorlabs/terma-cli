package install

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Removal is what an uninstall takes out of a workspace: terma's hook wiring, each agent's
// committed hooks, the checkout's own binding, its git configuration and session state.
// The home directory's keys and routing records stay: other checkouts of the project
// share them.
type Removal struct {
	Root, GitDir string
	Existing     *termaproject.File
	Hooks        hookmgr.Plan
	Agents       []hookmgr.Plan
	// RestoresHooksPath is core.hooksPath pointing at terma's shims.
	RestoresHooksPath bool
}

// PlanRemoval plans the uninstall of the workspace at root from its own binding only,
// never a linked worktree's main checkout's.
func PlanRemoval(ctx context.Context, reg *agents.Registry, root, gitDir string) (Removal, error) {
	for _, path := range append([]string{termaproject.FileName}, reg.HooksPaths()...) {
		if err := termaproject.CheckPath(root, path); err != nil {
			return Removal{}, err
		}
	}
	existing, err := termaproject.Load(root)
	if err != nil && !errors.Is(err, termaproject.ErrNotFound) {
		return Removal{}, err
	}
	r := Removal{Root: root, GitDir: gitDir, Existing: existing}
	det := hookmgr.Detect(root)
	if existing != nil && existing.Install.HookManager != "" {
		det.Manager = hookmgr.Manager(existing.Install.HookManager)
		if det.Manager == hookmgr.GitShim {
			det.ConfigPath = hookmgr.ShimDir
		}
	}
	if gitDir != "" {
		if r.Hooks, err = hookmgr.PlanUninstall(root, det); err != nil {
			return Removal{}, err
		}
		r.RestoresHooksPath = gitx.ConfigGet(ctx, root, "core.hooksPath") == hookmgr.ShimDir
	}
	if err := hookmgr.Validate(root, r.Hooks); err != nil {
		return Removal{}, err
	}
	if r.Agents, err = PlanAdapters(reg, root, reg.Names(), false); err != nil {
		return Removal{}, err
	}
	return r, nil
}

// Changes are the file changes, hook wiring first.
func (r Removal) Changes() []hookmgr.Change {
	changes := append([]hookmgr.Change{}, r.Hooks.Changes...)
	for _, p := range r.Agents {
		changes = append(changes, p.Changes...)
	}
	return changes
}

// Empty means nothing of terma's is installed.
func (r Removal) Empty() bool { return len(r.Changes()) == 0 && r.Existing == nil }

// Apply removes it all; warn hears of a repository policy that could not be removed,
// which never stops the rest.
func (r Removal) Apply(ctx context.Context, reg *agents.Registry, warn func(string)) error {
	if err := hookmgr.Apply(r.Root, r.Hooks); err != nil {
		return err
	}
	for _, p := range r.Agents {
		if err := hookmgr.Apply(r.Root, p); err != nil {
			return err
		}
	}
	// Every scoped harness, not only the ones an install chose.
	for _, h := range reg.Harnesses() {
		local, ok := h.Local(r.Root)
		if !ok {
			continue
		}
		if _, err := local.Disconnect(); err != nil {
			warn(fmt.Sprintf("could not remove %s's repository policy: %v", h.DisplayName(), err))
		}
	}
	// Routing records and keys are per project, shared with other checkouts, so they
	// stay; removing the binding is what stops routing here.
	if r.GitDir != "" {
		if err := Unwire(ctx, r.Root, r.GitDir); err != nil {
			return err
		}
	}
	if err := termaproject.Remove(r.Root); err != nil {
		return err
	}
	stateDir, err := termaproject.StateDir(r.Root, r.GitDir)
	if err != nil {
		return err
	}
	if err := session.Open(stateDir).Remove(); err != nil {
		return err
	}
	if stateDir != r.GitDir {
		// A workspace that gained Git keeps private session storage, but its hook
		// restoration journal lives in the worktree metadata.
		if r.GitDir != "" {
			if err := session.Open(r.GitDir).Remove(); err != nil {
				return err
			}
		}
		// Remove the reservation only when empty; leave any unrelated files.
		_ = os.Remove(stateDir)
	}
	return nil
}
