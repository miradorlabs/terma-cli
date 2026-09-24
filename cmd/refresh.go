package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// A refresh brings what earlier versions of terma wrote up to this build, so an update
// takes effect without re-running `terma install`. Re-running install is not the same
// thing: several of its choices are flags it never records (--signals, --exclude-prompts,
// --identity, --no-statusline, --activation), so a bare re-run would put them back to
// their defaults, and it signs in. A refresh works only from what is on disk. It
// rewrites files terma wrote with this build's templates, never creates one, never signs
// in, and never touches a choice: an absent shim, status line or hook file stays absent.

// refreshMachine rewrites the home-directory files every repository shares: the PATH
// shims, the wrapped Claude status line and the OpenCode plugin. It returns the paths
// it changed, carrying on past a failure so one broken file does not strand the rest.
func refreshMachine() ([]string, error) {
	changed, err := shim.RefreshShims()
	errs := []error{err}
	if path, ok, err := (harness.Claude{}).RefreshStatusLine(); err != nil {
		errs = append(errs, fmt.Errorf("claude status line: %w", err))
	} else if ok {
		changed = append(changed, path)
	}
	if path, ok, err := (harness.OpenCode{}).RefreshPlugin(); err != nil {
		errs = append(errs, fmt.Errorf("opencode plugin: %w", err))
	} else if ok {
		changed = append(changed, path)
	}
	return changed, errors.Join(errs...)
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
	plans, err := planAdapters(root, adapter.WiredNames(root), true)
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		r.plan.agents = append(r.plan.agents, existingFilesOnly(p))
	}
	return r, nil
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
	_ = selfupdate.SaveRefreshed(dir, Version)
}
