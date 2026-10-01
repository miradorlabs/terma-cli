package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// A refresh brings what earlier versions of terma wrote up to this build, so an update
// takes effect without re-running `terma install`. Re-running install is not the same
// thing: several of its choices are flags it never records (--signals, --exclude-prompts,
// --identity, --no-statusline), so a bare re-run would put them back to their
// defaults, and it signs in. A refresh works only from what is on disk. It rewrites
// files terma wrote with this build's templates, never creates one, never signs in, and
// never touches a choice: an absent status line or hook file stays absent.

// refreshMachine rewrites the home-directory files every repository shares, each agent's
// (agents.MachineRefresher). It returns the paths it changed, carrying on past a failure
// so one broken file does not strand the rest.
func (app *App) refreshMachine() ([]string, error) {
	var changed []string
	var errs []error
	for _, a := range app.agents.With[agents.MachineRefresher]() {
		if path, ok, err := a.RefreshMachine(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		} else if ok {
			changed = append(changed, path)
		}
	}
	return changed, errors.Join(errs...)
}

// repoRefresh is what a refresh would change in one repository's committed files.
type repoRefresh struct {
	root  string
	plan  install.HookPlan
	notes []string
}

// planRepoRefresh plans the refresh of the repository around the working directory,
// from its binding: the commit hooks through the manager it records, and the agent hook
// files its hooks files already wire (adapter.WiredNames). It returns nil outside a
// workspace terma installed; a workspace outside Git has agent hooks and no commit hooks.
// Only files that exist are refreshed; one that is gone was removed by someone, and
// bringing it back is `terma install`'s decision.
func (app *App) planRepoRefresh(ctx context.Context) (*repoRefresh, error) {
	root, gitDir, err := workspaceHere(ctx)
	if err != nil {
		return nil, nil
	}
	// A linked worktree refreshes its own hook files from its main checkout's binding
	// when it has none of its own: the committed files are the same repository's.
	existing, _, err := termaproject.Resolve(root, gitDir)
	if errors.Is(err, termaproject.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &repoRefresh{root: root}
	var det hookmgr.Detection
	if recorded := existing.Install.HookManager; recorded != "" && gitDir != "" {
		if det = hookmgr.Detect(root); string(det.Manager) != recorded {
			r.notes = append(r.notes, fmt.Sprintf("The commit hooks were installed through %s, but the repository now uses %s. Run `terma install` to move them.", recorded, det.Manager))
			det = hookmgr.Detection{}
		}
	}
	plan, err := install.PlanHooks(app.agents, root, det, app.agents.WiredNames(root))
	if err != nil {
		return nil, err
	}
	r.plan = plan.ExistingOnly()
	return r, nil
}

// stampVersion records this build as the terma that last wrote the repository's
// committed files, in the checkout's own binding (a linked worktree reading its main
// checkout's has none to stamp). It reports whether the file changed.
func (app *App) stampVersion(root string) (bool, error) {
	bound, err := termaproject.Load(root)
	if errors.Is(err, termaproject.ErrNotFound) {
		return false, nil
	}
	if err != nil || bound.Install.Version == app.version {
		return false, err
	}
	bound.Install.Version = app.version
	return true, termaproject.Save(root, bound)
}

// runRefresh is `terma update --refresh`: the machine's files, then the repository
// around the working directory, reported as it goes. Saved state is migrated first,
// retrying a migration that failed, so the files are rewritten from current state.
func (app *App) runRefresh(ctx context.Context, out io.Writer) error {
	out = style.Highlight(out)
	var migrateErr error
	if dir, err := config.Dir(); err == nil {
		var applied []string
		applied, migrateErr = migrate.Run(ctx, dir, true)
		for _, name := range applied {
			fmt.Fprintf(out, "Migrated saved state: %s.\n", name)
		}
		if migrateErr != nil {
			migrateErr = fmt.Errorf("migrate saved state: %w", migrateErr)
		}
	}
	machine, machineErr := app.refreshMachine()
	repo, repoErr := app.planRepoRefresh(ctx)
	var repoChanged []string
	if repo != nil && !repo.plan.Empty() {
		if repoErr = repo.plan.Apply(repo.root); repoErr == nil {
			repoChanged = repo.plan.Paths()
			if stamped, err := app.stampVersion(repo.root); err != nil {
				repoErr = err
			} else if stamped {
				repoChanged = append(repoChanged, termaproject.FileName)
			}
		}
	}

	switch {
	case len(machine) == 0 && len(repoChanged) == 0:
		fmt.Fprintf(out, "Nothing to refresh: what terma installed already matches %s.\n", app.version)
	default:
		fmt.Fprintf(out, "Refreshed for terma %s:\n", app.version)
		for _, p := range machine {
			fmt.Fprintf(out, "  updated %s\n", p)
		}
		for _, p := range repoChanged {
			fmt.Fprintf(out, "  updated %s\n", p)
		}
	}
	if repo != nil {
		for _, n := range repo.notes {
			fmt.Fprintf(out, "\n%s\n", n)
		}
	}
	if len(repoChanged) > 0 {
		fmt.Fprintln(out)
		printCommitList(out, "These are committed files. Commit them so every clone runs the same hooks:", repoChanged)
		for _, a := range app.agents.With[agents.Retrusting]() {
			if slices.Contains(repoChanged, a.HooksPath()) {
				fmt.Fprintf(out, "\n%s\n", a.RetrustNote())
			}
		}
	}
	if repo == nil {
		fmt.Fprintln(out, "\nRun `terma update --refresh` inside each repository terma is installed in to update its committed hooks too.")
	}
	err := errors.Join(migrateErr, machineErr, repoErr)
	if err == nil {
		if dir, dirErr := config.Dir(); dirErr == nil {
			err = selfupdate.SaveRefreshed(dir, app.version)
		}
	}
	return err
}

// refreshAfterUpgrade runs the first time an interactive command runs under a release
// newer than the one that last refreshed this machine — however it got here: `terma
// update`, an automatic update, or Homebrew or npm on their own. It refreshes only the
// home-directory files; committed files change only when the developer asks, so for the
// current repository it says what is out of date instead. It stays quiet when nothing
// changed, and records the release either way so it runs once.
func (app *App) refreshAfterUpgrade(ctx context.Context, dir string, out io.Writer) {
	out = style.Highlight(out)
	if !selfupdate.NeedsRefresh(dir, app.version) {
		return
	}
	changed, err := app.refreshMachine()
	if len(changed) > 0 {
		fmt.Fprintf(out, "terma %s refreshed %d file(s) an earlier version installed.\n", app.version, len(changed))
	}
	if err != nil {
		fmt.Fprintf(out, "terma %s could not refresh what an earlier version installed (%v). Run `terma update --refresh` to retry.\n", app.version, err)
	}
	if repo, _ := app.planRepoRefresh(ctx); repo != nil && !repo.plan.Empty() {
		fmt.Fprintln(out, "This repository's hooks were written by an earlier terma. Run `terma update --refresh` here to update them.")
	}
	if err == nil {
		_ = selfupdate.SaveRefreshed(dir, app.version)
	}
}
