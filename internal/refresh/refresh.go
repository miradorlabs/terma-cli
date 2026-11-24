// Package refresh rewrites, with this build's templates, the home-directory files an
// earlier terma wrote: from disk alone, never creating a file, signing in, or changing a
// choice, which `terma setup` would; but an agent's home that a client made after setup gets
// terma's hook when another home of that agent already has it switched on, and what an
// earlier terma kept in the state directory is brought to what this build reads (Migrate).
package refresh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// Refresher refreshes as one terma build.
type Refresher struct {
	// StateDir is terma's state directory, which records the release that last refreshed.
	StateDir string
	Agents   *agents.Registry
	Version  string
	// RelayService rewrites the relay's service when it is not what this build would
	// install, returning its path and whether it did; nil leaves the service alone.
	RelayService func(ctx context.Context) (path string, changed bool, err error)
	// Migrate brings what an earlier terma kept in the state directory to what this build
	// reads, from disk alone, returning the files it wrote; nil migrates nothing.
	Migrate func(ctx context.Context) (wrote []string, err error)
}

// machine refreshes the agents' home-directory files, then the relay's service, which an
// earlier build may have written to run a command this one lacks, then, once no relay of an
// earlier build is in the way, the state an earlier build kept.
func (r Refresher) machine(ctx context.Context) ([]string, error) {
	changed, err := r.Machine()
	var serviceErr error
	if r.RelayService != nil {
		var path string
		var ok bool
		if path, ok, serviceErr = r.RelayService(ctx); serviceErr != nil {
			err = errors.Join(err, fmt.Errorf("relay service: %w", serviceErr))
		} else if ok {
			changed = append(changed, path)
		}
	}
	// A relay step that failed may have left an earlier build's relay running: the next
	// refresh migrates, once it is gone.
	if r.Migrate != nil && serviceErr == nil {
		wrote, migrateErr := r.Migrate(ctx)
		changed = append(changed, wrote...)
		if migrateErr != nil {
			err = errors.Join(err, fmt.Errorf("state of an earlier version: %w", migrateErr))
		}
	}
	return changed, err
}

// Machine rewrites each agent's home-directory files, carrying on past a failure.
func (r Refresher) Machine() ([]string, error) {
	var changed []string
	var errs []error
	refreshed := map[string]bool{}
	for _, a := range r.Agents.With[agents.MachineRefresher]() {
		refreshed[a.Name()] = true
		if path, ok, err := a.RefreshMachine(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		} else if ok {
			changed = append(changed, path)
		}
	}
	for _, a := range r.Agents.With[agents.UserHooks]() {
		if refreshed[a.Name()] || !r.Agents.IsSupported(a.Name()) {
			continue
		}
		if path, ok, err := refreshUserHooks(a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		} else if ok {
			changed = append(changed, path)
		}
	}
	return changed, errors.Join(errs...)
}

// refreshUserHooks updates an already-installed machine-wide hooks file for a to this
// build's hook set without creating a file that does not exist or changing an agent terma
// did not install into.
func refreshUserHooks(a agents.UserHooks) (string, bool, error) {
	path, err := a.UserHooksPath()
	if err != nil || path == "" {
		return "", false, err
	}
	data, err := hookmgr.ReadFile(path)
	if err != nil || data == nil {
		return path, false, err
	}
	termas := hookmgr.TermaPaths(data)
	if len(termas) != 1 {
		return path, false, nil
	}
	dir := filepath.Dir(path)
	cmd := hookmgr.UserHookCommand(termas[0])
	plan, err := a.PlanUserHooks(dir, cmd, true)
	if err != nil {
		return path, false, err
	}
	changed := false
	if !plan.Empty() {
		if err := hookmgr.ApplyThroughLinks(dir, plan); err != nil {
			return path, false, err
		}
		changed = true
	}
	if t, ok := a.(agents.HookTrusting); ok {
		done, err := t.SyncHookTrust(path, cmd)
		if err != nil {
			return path, changed, err
		}
		if done.Approved > 0 || done.Withdrawn > 0 {
			changed = true
		}
	}
	return path, changed, nil
}

// Run is `terma update --refresh`: it refreshes the machine's files and returns the paths
// it changed. Only a refresh with no error records this build as refreshed.
func (r Refresher) Run(ctx context.Context) ([]string, error) {
	changed, err := r.machine(ctx)
	if err == nil {
		err = selfupdate.SaveRefreshed(r.StateDir, r.Version)
	}
	return changed, err
}

// Upgrade is what the first command under a newer release refreshed.
type Upgrade struct {
	// Due is false when this build has refreshed before, and nothing was done.
	Due     bool
	Changed []string
}

// AfterUpgrade runs once per newer release, however it was installed: it refreshes the
// home-directory files.
func (r Refresher) AfterUpgrade(ctx context.Context) (Upgrade, error) {
	if !selfupdate.NeedsRefresh(r.StateDir, r.Version) {
		return Upgrade{}, nil
	}
	up := Upgrade{Due: true}
	changed, err := r.machine(ctx)
	up.Changed = changed
	if err == nil {
		_ = selfupdate.SaveRefreshed(r.StateDir, r.Version)
	}
	return up, err
}
