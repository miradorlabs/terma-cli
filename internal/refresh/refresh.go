// Package refresh rewrites, with this build's templates, the home-directory files an
// earlier terma wrote: from disk alone, never creating a file, signing in, or changing a
// choice, which `terma setup` would.
package refresh

import (
	"context"
	"errors"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/agents"
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
}

// machine refreshes the agents' home-directory files, then the relay's service, which an
// earlier build may have written to run a command this one lacks.
func (r Refresher) machine(ctx context.Context) ([]string, error) {
	changed, err := r.Machine()
	if r.RelayService == nil {
		return changed, err
	}
	path, ok, serviceErr := r.RelayService(ctx)
	if serviceErr != nil {
		serviceErr = fmt.Errorf("relay service: %w", serviceErr)
	} else if ok {
		changed = append(changed, path)
	}
	return changed, errors.Join(err, serviceErr)
}

// Machine rewrites each agent's home-directory files, carrying on past a failure.
func (r Refresher) Machine() ([]string, error) {
	var changed []string
	var errs []error
	for _, a := range r.Agents.With[agents.MachineRefresher]() {
		if path, ok, err := a.RefreshMachine(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		} else if ok {
			changed = append(changed, path)
		}
	}
	return changed, errors.Join(errs...)
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
