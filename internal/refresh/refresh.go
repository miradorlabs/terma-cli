// Package refresh rewrites, with this build's templates, the home-directory files an
// earlier terma wrote: from disk alone, never creating a file, signing in, or changing a
// choice, which `terma setup` would.
package refresh

import (
	"context"
	"errors"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// Refresher refreshes as one terma build.
type Refresher struct {
	Agents  *agents.Registry
	Version string
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

// Result is what a refresh did.
type Result struct {
	Migrated []string
	Machine  []string
}

// Run is `terma update --refresh`: pending migrations, then the machine's files. Only a
// refresh with no error records this build as refreshed.
func (r Refresher) Run(ctx context.Context) (Result, error) {
	var res Result
	var migrateErr error
	if dir, err := config.Dir(); err == nil {
		res.Migrated, migrateErr = migrate.Run(ctx, dir, true)
		if migrateErr != nil {
			migrateErr = fmt.Errorf("migrate saved state: %w", migrateErr)
		}
	}
	machine, machineErr := r.machine(ctx)
	res.Machine = machine
	err := errors.Join(migrateErr, machineErr)
	if err == nil {
		if dir, dirErr := config.Dir(); dirErr == nil {
			err = selfupdate.SaveRefreshed(dir, r.Version)
		}
	}
	return res, err
}

// Upgrade is what the first command under a newer release refreshed.
type Upgrade struct {
	// Due is false when this build has refreshed before, and nothing was done.
	Due     bool
	Changed []string
}

// AfterUpgrade runs once per newer release, however it was installed: it refreshes the
// home-directory files under dir.
func (r Refresher) AfterUpgrade(ctx context.Context, dir string) (Upgrade, error) {
	if !selfupdate.NeedsRefresh(dir, r.Version) {
		return Upgrade{}, nil
	}
	up := Upgrade{Due: true}
	changed, err := r.machine(ctx)
	up.Changed = changed
	if err == nil {
		_ = selfupdate.SaveRefreshed(dir, r.Version)
	}
	return up, err
}
