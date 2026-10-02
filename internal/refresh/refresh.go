// Package refresh rewrites, with this build's templates, the files an earlier terma
// wrote: from disk alone, never creating a file, signing in, or changing a choice, which
// a bare `terma install` would.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/project"
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

// Repo is what a refresh would change in one repository's committed files.
type Repo struct {
	Root  string
	Plan  install.HookPlan
	Notes []string
}

// PlanRepo plans the refresh of the workspace at root (gitDir "" outside Git) from its
// binding and the hooks files that already exist (agents.Registry.WiredNames); nil for a
// workspace with no binding, or none at all (root "").
func (r Refresher) PlanRepo(root, gitDir string) (*Repo, error) {
	if root == "" {
		return nil, nil
	}
	// A linked worktree without a binding refreshes its own files from its main checkout's.
	existing, _, err := project.Resolve(root, gitDir)
	if errors.Is(err, project.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	repo := &Repo{Root: root}
	var det hookmgr.Detection
	if recorded := existing.Install.HookManager; recorded != "" && gitDir != "" {
		if det = hookmgr.Detect(root); string(det.Manager) != recorded {
			repo.Notes = append(repo.Notes, fmt.Sprintf("The commit hooks were installed through %s, but the repository now uses %s. Run `terma install` to move them.", recorded, det.Manager))
			det = hookmgr.Detection{}
		}
	}
	plan, err := install.PlanHooks(r.Agents, root, det, r.Agents.WiredNames(root))
	if err != nil {
		return nil, err
	}
	repo.Plan = plan.ExistingOnly()
	return repo, nil
}

// Stamp records this build in the checkout's own binding and reports whether it changed.
func (r Refresher) Stamp(root string) (bool, error) {
	bound, err := project.Load(root)
	if errors.Is(err, project.ErrNotFound) {
		return false, nil
	}
	if err != nil || bound.Install.Version == r.Version {
		return false, err
	}
	bound.Install.Version = r.Version
	return true, project.Save(root, bound)
}

// Result is what a refresh did.
type Result struct {
	Migrated []string
	Machine  []string
	// Repo is nil outside a bound workspace.
	Repo        *Repo
	RepoChanged []string
	// Retrust are what the agents whose rewritten hooks must be trusted again say.
	Retrust []string
}

// Run is `terma update --refresh`: pending migrations, the machine's files, then the
// repository's at root. Only a refresh with no error records this build as refreshed.
func (r Refresher) Run(ctx context.Context, root, gitDir string) (Result, error) {
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
	repo, repoErr := r.PlanRepo(root, gitDir)
	res.Repo = repo
	if repo != nil && !repo.Plan.Empty() {
		if repoErr = repo.Plan.Apply(repo.Root); repoErr == nil {
			res.RepoChanged = repo.Plan.Paths()
			if stamped, err := r.Stamp(repo.Root); err != nil {
				repoErr = err
			} else if stamped {
				res.RepoChanged = append(res.RepoChanged, project.FileName)
			}
		}
	}
	for _, a := range r.Agents.With[agents.Retrusting]() {
		if !slices.Contains(res.RepoChanged, a.HooksPath()) {
			continue
		}
		// Rewritten entries terma approves again itself; only a failure needs the developer.
		if t, ok := a.(agents.HookTrusting); ok {
			if _, err := t.SyncHookTrust(filepath.Join(repo.Root, filepath.FromSlash(a.HooksPath())), nil); err == nil {
				continue
			}
		}
		res.Retrust = append(res.Retrust, a.RetrustNote())
	}
	err := errors.Join(migrateErr, machineErr, repoErr)
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
	// RepoStale means the repository's committed hooks predate this build; they are only
	// reported, since committing them is the developer's call.
	RepoStale bool
}

// AfterUpgrade runs once per newer release, however it was installed: it refreshes the
// home-directory files under dir and only reports out-of-date committed ones at root.
func (r Refresher) AfterUpgrade(ctx context.Context, dir, root, gitDir string) (Upgrade, error) {
	if !selfupdate.NeedsRefresh(dir, r.Version) {
		return Upgrade{}, nil
	}
	up := Upgrade{Due: true}
	changed, err := r.machine(ctx)
	up.Changed = changed
	if repo, _ := r.PlanRepo(root, gitDir); repo != nil && !repo.Plan.Empty() {
		up.RepoStale = true
	}
	if err == nil {
		_ = selfupdate.SaveRefreshed(dir, r.Version)
	}
	return up, err
}
