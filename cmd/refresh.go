package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// A refresh brings what earlier versions of terma wrote up to this build, so an update
// takes effect without re-running `terma install`. Re-running install is not the same
// thing: several of its choices are flags it never records (--signals, --exclude-prompts,
// --identity, --no-statusline, --activation), so a bare re-run would put them back to
// their defaults, and it signs in. A refresh works only from what is on disk. It
// rewrites files terma wrote with this build's templates, never creates one, never signs
// in, and never touches a choice: an absent shim, status line or hook file stays absent.

// refreshMachine rewrites the home-directory files every repository shares, each agent's
// (agents.MachineRefresher). It also removes the PATH shims an earlier
// build installed — the local relay routes the agents now, and a shim left on PATH would
// run every agent launch through terma for nothing. It returns the paths it changed,
// carrying on past a failure so one broken file does not strand the rest.
func refreshMachine() ([]string, error) {
	var changed []string
	var errs []error
	if err := migrateLegacyExporters(); err != nil {
		errs = append(errs, fmt.Errorf("migrate PATH-shim exporters to the local relay: %w", err))
	} else if removed, err := shim.RemoveLegacy(); err != nil {
		errs = append(errs, fmt.Errorf("remove the PATH shims: %w", err))
	} else if removed {
		dir, _ := shim.ShimBinDir()
		changed = append(changed, dir+" — removed: the local relay routes the agents now")
	}
	for _, a := range registered.With[agents.MachineRefresher]() {
		if path, ok, err := a.RefreshMachine(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name(), err))
		} else if ok {
			changed = append(changed, path)
		}
	}
	return changed, errors.Join(errs...)
}

// Configure the replacement before removing an installed shim. Routing records
// retain the project's capture choices and keys; a refresh never rewrites them.
func migrateLegacyExporters() error {
	bin, err := shim.ShimBinDir()
	if err != nil {
		return err
	}
	installed, err := os.ReadDir(bin)
	if os.IsNotExist(err) || len(installed) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := routing.RoutingDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var agents []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		rec, ok, err := routing.LoadRecord(e.Name()[:len(e.Name())-5])
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, a := range rec.Harnesses {
			if !slices.Contains(agents, a) {
				agents = append(agents, a)
			}
		}
	}
	if err := connectMachineRelay(context.Background(), agents, "", relayReport{
		ok: func(string, string) {}, warn: func(string, string) {}, then: func(string) {}, detail: io.Discard,
	}); err != nil {
		return err
	}
	if len(registered.RelayTargets(agents)) == 0 {
		return nil
	}
	relayPath, err := relayDir()
	if err != nil {
		return err
	}
	if _, running, err := relayStats(relayPath); err != nil {
		return fmt.Errorf("verify the replacement relay: %w", err)
	} else if !running {
		return errors.New("the replacement relay is not running; keeping the PATH shims")
	}
	return nil
}

// repoRefresh is what a refresh would change in one repository's committed files.
type repoRefresh struct {
	root  string
	plan  hookPlan
	notes []string
}

// planRepoRefresh plans the refresh of the repository around the working directory,
// from its binding: the commit hooks through the manager it records, and the agent hook
// files its hooks files already wire (adapter.WiredNames). It returns nil outside a
// workspace terma installed; a workspace outside Git has agent hooks and no commit hooks.
// Only files that exist are refreshed; one that is gone was removed by someone, and
// bringing it back is `terma install`'s decision.
func planRepoRefresh(ctx context.Context) (*repoRefresh, error) {
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
	if recorded := existing.Install.HookManager; recorded != "" && gitDir != "" {
		det := hookmgr.Detect(root)
		if string(det.Manager) != recorded {
			r.notes = append(r.notes, fmt.Sprintf("The commit hooks were installed through %s, but the repository now uses %s. Run `terma install` to move them.", recorded, det.Manager))
		} else {
			hooks, err := hookmgr.PlanInstall(root, det)
			if err != nil {
				return nil, err
			}
			r.plan.det, r.plan.hooks = det, existingFilesOnly(hooks)
		}
	}
	plans, err := planAdapters(root, registered.WiredNames(root), true)
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		r.plan.agents = append(r.plan.agents, existingFilesOnly(p))
	}
	return r, nil
}

// stampVersion records this build as the terma that last wrote the repository's
// committed files, in the checkout's own binding (a linked worktree reading its main
// checkout's has none to stamp). It reports whether the file changed.
func stampVersion(root string) (bool, error) {
	bound, err := termaproject.Load(root)
	if errors.Is(err, termaproject.ErrNotFound) {
		return false, nil
	}
	if err != nil || bound.Install.Version == Version {
		return false, err
	}
	bound.Install.Version = Version
	return true, termaproject.Save(root, bound)
}

// existingFilesOnly keeps a plan's changes to files that are already there. The notes
// are for a first install (what each clone must run) and are dropped.
func existingFilesOnly(p hookmgr.Plan) hookmgr.Plan {
	p.Changes = slices.DeleteFunc(slices.Clone(p.Changes), func(c hookmgr.Change) bool { return c.Before == nil })
	p.Notes = nil
	return p
}

// runRefresh is `terma update --refresh`: the machine's files, then the repository
// around the working directory, reported as it goes. Saved state is migrated first,
// retrying a migration that failed, so the files are rewritten from current state.
func runRefresh(ctx context.Context, out io.Writer) error {
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
	machine, machineErr := refreshMachine()
	repo, repoErr := planRepoRefresh(ctx)
	var repoChanged []string
	if repo != nil && !repo.plan.empty() {
		if repoErr = repo.plan.apply(repo.root); repoErr == nil {
			repoChanged = repo.plan.paths()
			if stamped, err := stampVersion(repo.root); err != nil {
				repoErr = err
			} else if stamped {
				repoChanged = append(repoChanged, termaproject.FileName)
			}
		}
	}

	switch {
	case len(machine) == 0 && len(repoChanged) == 0:
		fmt.Fprintf(out, "Nothing to refresh: what terma installed already matches %s.\n", Version)
	default:
		fmt.Fprintf(out, "Refreshed for terma %s:\n", Version)
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
		if slices.Contains(repoChanged, hookmgr.CodexHooksPath) {
			fmt.Fprintln(out, "\nCodex runs changed hooks only after you trust them again in Codex; `terma doctor` names any it is skipping.")
		}
	}
	if repo == nil {
		fmt.Fprintln(out, "\nRun `terma update --refresh` inside each repository terma is installed in to update its committed hooks too.")
	}
	err := errors.Join(migrateErr, machineErr, repoErr)
	if err == nil {
		if dir, dirErr := config.Dir(); dirErr == nil {
			err = selfupdate.SaveRefreshed(dir, Version)
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
func refreshAfterUpgrade(ctx context.Context, dir string, out io.Writer) {
	out = style.Highlight(out)
	if !selfupdate.NeedsRefresh(dir, Version) {
		return
	}
	changed, err := refreshMachine()
	if len(changed) > 0 {
		fmt.Fprintf(out, "terma %s refreshed %d file(s) an earlier version installed.\n", Version, len(changed))
	}
	if err != nil {
		fmt.Fprintf(out, "terma %s could not refresh what an earlier version installed (%v). Run `terma update --refresh` to retry.\n", Version, err)
	}
	if repo, _ := planRepoRefresh(ctx); repo != nil && !repo.plan.empty() {
		fmt.Fprintln(out, "This repository's hooks were written by an earlier terma. Run `terma update --refresh` here to update them.")
	}
	if err == nil {
		_ = selfupdate.SaveRefreshed(dir, Version)
	}
}
