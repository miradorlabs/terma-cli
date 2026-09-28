package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/serverkey"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type installFlags struct {
	projectRef string
	harnesses  string
	adapters   string
	// activation is how per-repo agent routing is delivered: "shim", "wrapper", or ""
	// (ask). Codex and Claude Code route to this repo's project through it.
	activation   string
	noHooks      bool
	noDoctor     bool
	noStatusLine bool
	// noPath keeps install out of the shell startup file: it prints the PATH line for the
	// developer to place instead of writing it.
	noPath   bool
	identity string
	signals  string
	// prompts is --prompts: "on", "off", or "" (keep what this developer chose for the
	// project last time, on for a first install).
	prompts            string
	excludePrompts     bool
	excludeToolContent bool
	updatePolicy       bool
	noBrowser          bool
	dryRun             bool
	assumeYes          bool
	force              bool
	// verbose prints the long-form account of every step under the checklist.
	verbose bool
}

func newInstallCommand() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Configure this workspace: point your agents at its project and wire the hooks",
		Long: `Run once per repository. install is self-contained — it signs you in if you have
not run ` + "`terma setup`" + `, asks which agents you use if you have not chosen, then:

  1. Binds the repository to a Terma project and records it in .terma/settings.json —
     committed, no secrets. An organization with one project is bound to it without
     asking. With several, on a terminal you choose the project every time, the one
     already bound offered first (Enter keeps it); --project names it instead, and
     without a terminal, or with --yes, an existing binding is kept. A binding to a
     project your account cannot see is not used: install says why and chooses again.
  2. Points each of your agents at that project, per repository:
       - Claude Code exports to it through per-repo settings (claude --settings);
       - Codex CLI exports to it through runtime -c overrides;
     both delivered by PATH shims, which install puts on PATH at the end of your
     shell's startup file (--no-path prints the line instead; --activation wrapper
     prints shell functions instead). Keys stay in your home directory, namespaced by
     project — never in the repository. Prompt text and model responses are sent
     (your last choice for the project, on for a first install); --prompts off stops
     them.
     Codex Desktop reports through repository hooks and the local Terma spool.
  3. Enables repository telemetry, including for machines configured to export only
     from installed repositories. Existing repository policies are preserved unless
     --signals or a content flag changes them.
  4. Offers to install the commit hooks and the agents' own hooks (session start/end,
     tool use, stop) into the files you commit, so one merged PR onboards everyone.

Run from any subdirectory of a Git worktree; install uses its root. Outside Git,
the first install uses the current directory and later installs find the nearest
.terma/settings.json above it. Agent hooks and telemetry work there; Git hooks and
commit stamping are skipped. Recognizable non-Git repositories produce a warning:
only Git has version-control integration. Bare repositories are not workspaces.

The keys and per-project configuration live in your home directory; the committed
.terma/settings.json only names the project.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.updatePolicy = cmd.Flags().Changed("signals") || cmd.Flags().Changed("prompts") || cmd.Flags().Changed("exclude-prompts") || cmd.Flags().Changed("exclude-tool-content")
			return runInstall(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.projectRef, "project", "", "Terma project (name or id) to bind the repository to")
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to configure ("+strings.Join(availableAgentNames(), ", ")+"); default: what `terma setup` recorded, else a picker")
	cmd.Flags().StringVar(&f.adapters, "adapters", "", "comma-separated agents whose committed hooks to wire (default: the configured agents that have one)")
	cmd.Flags().StringVar(&f.activation, "activation", "", "per-repo routing delivery: shim (PATH shims, the default) or wrapper (printed shell functions)")
	cmd.Flags().BoolVar(&f.noHooks, "no-hooks", false, "do not install commit hooks or agent hooks")
	cmd.Flags().BoolVar(&f.noDoctor, "no-doctor", false, "do not run `terma doctor` to verify the chain after installing")
	cmd.Flags().BoolVar(&f.noPath, "no-path", false, "do not add the shim directory to PATH in your shell's startup file; print the line instead")
	cmd.Flags().BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap Claude Code's status line (which captures the plan's rate-limit windows)")
	cmd.Flags().StringVar(&f.identity, "identity", "", "identity stamped on Codex/OpenCode sessions (default: git user.email; \"none\" to omit)")
	cmd.Flags().StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
	cmd.Flags().StringVar(&f.prompts, "prompts", "", "send prompt text and model responses: on or off (default: your last choice for this project, on for a first install)")
	cmd.Flags().BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	// --exclude-prompts is --prompts off, kept working for the scripts that pass it.
	_ = cmd.Flags().MarkHidden("exclude-prompts")
	cmd.Flags().BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "show what would change without writing anything")
	cmd.Flags().BoolVar(&f.verbose, "verbose", false, "say what each step wrote, not only whether it worked")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace conflicting harness settings instead of refusing")
	return cmd
}

func runInstall(cmd *cobra.Command, f installFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	root, gitDir, err := workspaceHere(ctx)
	if err != nil {
		return err
	}
	if gitDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if vcs := termaproject.UnsupportedVCS(cwd); vcs != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: detected %s repository metadata. Terma only supports Git for version-control integration; this repository type is not supported. Agent hooks and telemetry can still be installed, but commit hooks and commit stamping are skipped.\n", vcs)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A dry run is nothing but its plan, so it always says everything.
	ui := newInstallUI(out, f.verbose || f.dryRun)
	fmt.Fprintf(out, "%s in %s\n\n", ui.p.Bold("Installing terma"), tildePath(root))
	for _, path := range []string{termaproject.FileName, hookmgr.ClaudeSettingsPath} {
		if err := termaproject.CheckPath(root, path); err != nil {
			return err
		}
	}
	// A linked worktree installing for the first time keeps its main checkout's project
	// rather than asking again; the binding it writes is its own.
	existing, _, err := termaproject.Resolve(root, gitDir)
	if err != nil && !errors.Is(err, termaproject.ErrNotFound) {
		return err
	}

	// 1. Which agents to configure: --harness, else recorded, else a picker. Resolved
	// before sign-in so we know whether sign-in is even needed.
	agents, err := resolveInstallHarnesses(cmd, cfg, f)
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	if err != nil {
		return err
	}
	if slices.Contains(agents, codexDesktopAgent) {
		signals, err := harness.ParseSignals(f.signals)
		if err != nil {
			return err
		}
		if !slices.Contains(signals, harness.SignalLogs) {
			return errors.New("codex desktop needs the logs signal to route sessions by repository")
		}
	}

	// 2. Auth, lazily. install signs in only when a step needs a credential — a
	// telemetry harness to point (mint a key), or a project to look up by name or in a
	// picker. A hooks-only install against a verbatim project id needs none, and a
	// server key (TERMA_API_KEY) skips it entirely. A --dry-run never signs in: sign-in
	// verifies and rewrites the stored credential, which "nothing written" forbids, so a
	// dry run plans against whatever credential is already present and says so.
	needsAuth := cfg.APIKey == "" && installNeedsAuth(agents, f.projectRef, existing, !f.noHooks)
	if needsAuth && !f.dryRun {
		if cfg, err = signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser}); err != nil {
			return err
		}
	}

	// 3. Project binding.
	b, err := resolveBinding(cmd, cfg, existing, f.projectRef, needsAuth, !f.assumeYes && !f.dryRun && canPrompt())
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	if err != nil {
		// A dry run never signs in (step 2), so when no credential is stored the
		// project picker cannot reach the API to resolve a binding. Rather than fail
		// before printing anything, plan against an unresolved project: the plan's
		// files (hooks, adapters) do not depend on the project id, and the dry run
		// already says a real install would sign in first. Any other error is real.
		if !f.dryRun || !errors.Is(err, auth.ErrNotLoggedIn) {
			return err
		}
		b = binding{}
	}
	// Point the resolved config at the repo's project so key minting and resource
	// attributes speak for it.
	cfg.ProjectID, cfg.ProjectName, cfg.OrganizationID = b.ID, b.Name, b.OrganizationID

	if gitDir == "" {
		ui.warn("Git hooks", "skipped — not a Git repository, so commits are not stamped")
	}
	env := ""
	if cfg.Environment != config.EnvProd {
		env = " (" + cfg.Environment + ")"
	}
	if b.ID == "" && b.Name == "" {
		ui.warn("Project", "unresolved — a real install signs in and selects one"+env)
	} else {
		ui.ok("Project", nameOrID(b.Name, b.ID)+env)
	}

	// Whether the developer's agents send what was said, settled here — once the project
	// is known, so the last choice for it stands — and carried as excludePrompts to the
	// routing record and any repository policy written below. Nothing asks, so the line
	// names the command that changes it.
	include, err := resolvePrompts(cmd, cfg.ProjectID, f)
	if err != nil {
		return err
	}
	f.excludePrompts = !include
	if len(exportingAgents(agents)) > 0 {
		if include {
			ui.ok("Prompts", "prompt text and model responses are sent — `terma install --prompts off` stops them")
		} else {
			ui.ok("Prompts", "prompt text and model responses are not sent — `terma install --prompts on` sends them")
		}
	}

	// The hook plan — the commit hooks and the agents' own hooks, into committed files —
	// is built once, before anything is written, so a dry run prints exactly the plan an
	// install goes on to apply. The wired adapters are a team decision, so a re-install
	// keeps every agent the repository's hooks files already wire unless --adapters
	// overrides it — a colleague re-running install must not rewrite the committed hooks
	// to match their own agent set.
	adapters := installAdapters(root, agents, f.adapters)
	if slices.Contains(agents, codexDesktopAgent) && !slices.Contains(adapters, shim.AgentCodex) {
		return errors.New("codex desktop needs the Codex repository hooks; include codex in --adapters")
	}
	det := hookmgr.Detect(root)
	if gitDir == "" {
		det = hookmgr.Detection{}
	}
	var plan hookPlan
	if !f.noHooks {
		if plan, err = planHooks(root, det, adapters); err != nil {
			return err
		}
	} else if slices.Contains(agents, codexDesktopAgent) {
		codexPlan, err := hookmgr.PlanCodexHooks(root, true)
		if err != nil {
			return err
		}
		if !codexPlan.Empty() {
			return errors.New("codex desktop needs the SessionStart repository hook; run `terma install` without --no-hooks")
		}
	}

	if f.dryRun {
		if !f.noHooks {
			plan.print(out)
		}
		if needsAuth {
			fmt.Fprintln(out, "\nA real install would sign in first (not done for a dry run).")
		}
		for _, h := range repoPolicyHarnesses(adapters) {
			path, err := h.(harness.Scoped).Local(root).ConfigPath()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nRepository telemetry: %s (preserve existing policy unless export flags are supplied).\n", path)
		}
		if slices.Contains(agents, codexDesktopAgent) {
			fmt.Fprintln(out, "\nCodex Desktop: a real install writes repository hooks and a local project route. Then open Settings → Hooks → Review in Codex Desktop to approve the Terma entries; Codex CLI is not required.")
		}
		fmt.Fprintln(out, "\nDry run: nothing written.")
		return nil
	}

	// Reserve the private store even before the first agent event. A hook already
	// in flight when git init runs and a hook starting afterwards must choose the
	// same store, including when neither has written a manifest yet.
	if gitDir == "" {
		stateDir, err := termaproject.StateDir(root, "")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return err
		}
	}

	// 4. Configure each agent for this repository (per-repo routing). This is the
	// per-developer half — keys and routing state in the home directory — and touches
	// no committed file.
	if err := connectHarnessesForRepo(cmd, ui, cfg, agents, f); err != nil {
		return err
	}

	// Wrap Claude Code's status line to capture the plan's rate-limit windows — the
	// strongest funding evidence a machine produces. It is machine-level (the user's
	// global Claude settings) and idempotent. Gated on Claude being a configured agent
	// (not merely installed) so it only touches the global config for a developer who
	// has chosen Claude — which also keeps `--harness none` installs from touching it.
	// --no-statusline opts out.
	if !f.noStatusLine && slices.Contains(agents, shim.AgentClaude) {
		if note, ok := installStatusLine(cmd.ErrOrStderr()); ok {
			fmt.Fprintf(ui.detail, "\n%s\n", note)
			ui.ok("Status line", "reads your plan's usage windows")
		} else {
			ui.warn("Status line", "not wrapped — your plan's usage windows are not captured")
		}
	}

	// 5. Hooks: apply the plan built above.
	installedHooks := gitDir != "" && existing != nil && existing.Install.HookManager != ""
	var written []string    // the repository files this run wrote hooks into, to commit
	var afterMerge []string // what each clone does once they are merged
	if !f.noHooks {
		plan.print(ui.detail)
		switch {
		case plan.empty():
			// An empty git-hook plan means the commit hooks are already wired, so this
			// repo is hook-installed; record them in the binding without rewriting.
			installedHooks = gitDir != ""
			ui.ok("Hooks", plan.summary(adapters)+" — already in place")
		case f.assumeYes || confirmYes(cmd, "Write terma's hooks to "+joinNames(plan.files())+"?", plan.explain()):
			if err := plan.apply(root); err != nil {
				return err
			}
			installedHooks = gitDir != ""
			written = plan.paths()
			ui.ok("Hooks", plan.summary(adapters))
			afterMerge = plan.hooks.Notes
		default:
			adapters = adapter.WiredNames(root) // declined: only what is already wired
			ui.warn("Hooks", "not written — commits are not stamped until they are")
			ui.then("Run `terma install` again and accept the hooks when you are ready.")
		}
	} else {
		adapters = adapter.WiredNames(root) // --no-hooks: only what is already wired
	}
	if slices.Contains(agents, codexDesktopAgent) {
		codexPlan, err := hookmgr.PlanCodexHooks(root, true)
		if err != nil {
			return err
		}
		if !codexPlan.Empty() {
			return errors.New("codex desktop needs the SessionStart repository hook; run `terma install` without --no-hooks and accept the Codex hook plan")
		}
	}

	// The key this machine delivers the repository's hook events with. Pointing a
	// telemetry agent stores one as a side effect; nothing else does, and `terma setup`
	// no longer connects anything — so without this step a developer whose agents are
	// all hooks-only would see "Installed." while every commit, tool call and observation
	// sat in the spool, held for a key that no command would ever mint.
	if installedHooks || len(adapter.WiredNames(root)) > 0 {
		if k := ensureSpoolKey(ctx, cfg); k.fix == "" {
			ui.ok("Hook events", k.state)
		} else {
			ui.warn("Hook events", k.state)
			ui.then(k.fix)
		}
	}

	// 6. Repository telemetry also supports developers using a global repos-only
	// connection. Hooks alone do not enable that connection's exporters.
	paths, err := writeRepoPolicy(ctx, ui, root, cfg, repoPolicyHarnesses(adapters), f)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !slices.Contains(written, path) {
			written = append(written, path)
		}
	}

	// 7. The committed binding. installed_at is the onboarder's and never moves;
	// terma_version is the terma that last wrote the committed files, so it moves only
	// when this run wrote one — a colleague's install that changes nothing does not
	// churn the file.
	version, installedAt := Version, time.Now().UTC()
	if existing != nil {
		if existing.Install.Version != "" && len(written) == 0 {
			version = existing.Install.Version
		}
		if !existing.Install.InstalledAt.IsZero() {
			installedAt = existing.Install.InstalledAt
		}
	}
	file := &termaproject.File{
		Project: termaproject.Project{
			ID:             b.ID,
			Name:           b.Name,
			OrganizationID: b.OrganizationID,
			Environment:    b.Environment,
		},
		Install: termaproject.Install{
			HookManager: managerOrEmpty(det, installedHooks),
			Hooks:       hooksOrNil(installedHooks),
			Version:     version,
			InstalledAt: installedAt,
		},
	}
	if err := termaproject.Save(root, file); err != nil {
		return err
	}

	// 8. Per-clone git wiring for the shim manager.
	if installedHooks {
		if err := wireRepo(ctx, ui.detail, root, file); err != nil {
			return err
		}
	}
	if slices.Contains(agents, codexDesktopAgent) {
		fmt.Fprintln(ui.detail, "Codex Desktop captures this repository through trusted hooks and Terma's existing spool.")
		ui.then("Approve Codex Desktop capture:\n" +
			"a. Open this repository in Codex Desktop and trust the project if prompted.\n" +
			"b. Open Settings → Hooks, then select Review for the entries from .codex/hooks.json.\n" +
			"c. Inspect and approve each Terma hook command for full capture. Codex CLI is not required.\n" +
			"d. Run `terma desktop status` to confirm 'Codex hooks: ready', then start a new Local task in this repository.")
		if global, err := (harness.Codex{}).Status(); err == nil && global.Connected {
			ui.warn("Codex", "also has a user-level exporter; it may send Desktop activity from other repositories")
		}
	}

	if gitDir != "" && len(written) > 0 {
		// Save always rewrites the binding.
		written = append(written, termaproject.FileName)
		ui.then(commitList(ui.p, "Commit these files and open a PR — merging it onboards the repository:", written))
	}
	for _, n := range afterMerge {
		ui.then("After merging: " + n)
	}
	// The first run of a newer release brings what earlier versions wrote on this machine
	// up to this build — the shims and wraps this run did not rewrite itself — before
	// doctor checks them, and records it, so the refresh that would otherwise follow the
	// command has nothing left to do. The repository's committed hooks went through the
	// plan above, which rewrites a stale file as it adds a missing one.
	if dir, err := config.Dir(); err == nil && selfupdate.NeedsRefresh(dir, Version) {
		changed, err := refreshMachine()
		for _, p := range changed {
			fmt.Fprintf(ui.detail, "  updated %s\n", p)
		}
		if err != nil {
			ui.warn("Refreshed", "some files an earlier terma installed could not be updated ("+err.Error()+")")
			ui.then("Run `terma update --refresh` to retry.")
		} else {
			if len(changed) > 0 {
				ui.ok("Refreshed", fmt.Sprintf("%d file(s) an earlier terma installed", len(changed)))
			}
			_ = selfupdate.SaveRefreshed(dir, Version)
		}
	}

	// Verify the chain right away. Skipped without a terminal (a script, CI) or with
	// --no-doctor, since doctor makes a scratch commit and a network round-trip; those
	// callers can run `terma doctor` themselves.
	if f.noDoctor || !canPrompt() {
		ui.then("Run `terma doctor` to verify the chain end to end.")
	} else {
		ui.verify(cmd)
	}
	ui.finish()
	return nil
}

// resolvePrompts decides whether the developer's agents send prompt text and model
// responses, without asking: --prompts (or the older --exclude-prompts) when given, else
// the choice this developer made for the project last time (its routing record), on for
// a first install. Re-running install without the flag used to switch prompts back on.
func resolvePrompts(cmd *cobra.Command, projectID string, f installFlags) (bool, error) {
	explicit := strings.ToLower(strings.TrimSpace(f.prompts))
	excluded := cmd.Flags().Changed("exclude-prompts") && f.excludePrompts
	switch explicit {
	case "on":
		if excluded {
			return false, errors.New("--prompts on and --exclude-prompts disagree; pass one")
		}
		return true, nil
	case "off":
		return false, nil
	case "":
	default:
		return false, fmt.Errorf("--prompts %q: want on or off", f.prompts)
	}
	if excluded {
		return false, nil
	}
	if rec, ok, err := shim.LoadRecord(projectID); err == nil && ok && projectID != "" {
		return rec.IncludePrompts, nil
	}
	return true, nil
}

// installNeedsAuth reports whether install must obtain a credential: to point a
// telemetry harness (mint or list a key), to resolve the project by name or in a picker,
// or to mint the key this machine delivers hook events with — which a developer whose
// agents are all hooks-only (Cursor, Antigravity) gets from nowhere else. A repository
// wired with no agent of the developer's own (`--harness none`) is never made to sign in
// for it: ensureSpoolKey mints when a credential is already there and says so when not.
func installNeedsAuth(agents []string, projectRef string, existing *termaproject.File, wantsHooks bool) bool {
	for _, a := range telemetryAgentNames(agents) {
		if _, err := harness.Lookup(a); err == nil {
			return true
		}
	}
	ref := strings.TrimSpace(projectRef)
	if (ref == "" && existing == nil) || (ref != "" && projectRefNeedsLookup(ref)) {
		return true
	}
	projectID := ref
	if projectID == "" {
		projectID = existing.Project.ID
	}
	return wantsHooks && len(agents) > 0 && keystore.Get(projectID) == ""
}

func telemetryAgentNames(agents []string) []string {
	var names []string
	for _, name := range agents {
		if name == codexDesktopAgent {
			name = shim.AgentCodex
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// projectRefNeedsLookup reports whether resolving --project needs the API: an empty ref
// means a picker, a name means a lookup, and only a bare valid id is taken verbatim.
func projectRefNeedsLookup(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	return !termaproject.ValidID(ref)
}

// spoolKey is how this machine delivers the repository's hook events: state says it, and
// fix, when set, is what the developer has to do before they are delivered.
type spoolKey struct{ state, fix string }

// ensureSpoolKey makes sure this machine holds a key for cfg's project, minting one when
// it has none and a credential to mint with. It never fails the install: the hooks and
// the binding are what the repository needs, and held events wait up to the spool's
// MaxAge for a key.
func ensureSpoolKey(ctx context.Context, cfg *config.Config) spoolKey {
	if keystore.Get(cfg.ProjectID) != "" {
		return spoolKey{state: "delivered with this project's key"}
	}
	const held = "held until this machine has a key for the project"
	if cfg.APIKey != "" {
		return spoolKey{held, "TERMA_API_KEY cannot mint a key for hook events: unset it and run `terma install` again."}
	}
	client, err := newClient(cfg)
	var key string
	if err == nil {
		key, _, err = client.CreateServerKey(ctx, cfg.ProjectID, "terma-cli@"+harness.Hostname(),
			"Created by terma install, for hook events")
	}
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return spoolKey{held, "Sign in with `terma setup`, then run `terma install` again, so hook events from this machine are delivered."}
	case err != nil:
		return spoolKey{held, "Minting a key for hook events failed (" + err.Error() + "); run `terma install` again."}
	}
	if err := keystore.Set(cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
		return spoolKey{held, "Storing the key for hook events failed (" + err.Error() + "); run `terma install` again."}
	}
	return spoolKey{state: "Project key stored for this machine (" + keystore.Mask(key) + ")"}
}

// resolveInstallHarnesses picks the agents to configure: the --harness flag ("none" for
// no agents), else the machine-level list `terma setup` recorded, else a picker
// (recorded to the profile so the next repo does not ask). Which agents a developer
// configures is a per-developer choice, so a prior install's committed binding does not
// decide it.
func resolveInstallHarnesses(cmd *cobra.Command, cfg *config.Config, f installFlags) ([]string, error) {
	if h := strings.TrimSpace(f.harnesses); h != "" {
		if strings.EqualFold(h, "none") {
			return nil, nil
		}
		return parseAgentList(f.harnesses)
	}
	if len(cfg.Harnesses) > 0 {
		chosen := map[string]bool{}
		for _, name := range cfg.Harnesses {
			chosen[name] = true
		}
		if names := selectedInRegistryOrder(chosen); len(names) > 0 {
			return names, nil
		}
	}
	names, err := chooseHarnesses(cmd, cfg, setupFlags{assumeYes: f.assumeYes})
	if err != nil {
		return nil, err
	}
	if f.dryRun {
		return names, nil
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { p.Harnesses = names }); err != nil {
		return nil, err
	}
	return names, nil
}

// connectHarnessesForRepo points each configured telemetry harness at cfg's project,
// per repository. Claude routes through a per-repo settings document handed to it as
// `--settings`, Codex through runtime -c overrides, and OpenCode through its own per-repo plugin; the wrapper or
// PATH-shim activation is set up once for the routable pair (Claude, Codex). It writes
// no committed file — keys and routing state live in the home directory.
func connectHarnessesForRepo(cmd *cobra.Command, ui *installUI, cfg *config.Config, agents []string, f installFlags) error {
	ctx := cmd.Context()
	out := ui.detail
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	telemetryAgents := exportingAgents(agents)
	if len(telemetryAgents) == 0 {
		return nil
	}

	fmt.Fprintln(out, "\nConfiguring agents for this project:")
	rec := shim.Record{
		ProjectID:          cfg.ProjectID,
		Endpoint:           cfg.OTLPURL,
		Signals:            signalStrings(signals),
		IncludePrompts:     !f.excludePrompts,
		IncludeToolContent: !f.excludeToolContent,
	}
	if slices.Contains(telemetryAgents, shim.AgentCodex) {
		cli := slices.Contains(agents, shim.AgentCodex)
		desktop := slices.Contains(agents, codexDesktopAgent)
		rec.CLI = cli
		rec.Desktop = desktop
	}
	for _, a := range telemetryAgents {
		h, _ := harness.Lookup(a)
		key, _, _, _, err := resolveKey(ctx, cfg, h, connectFlags{})
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		if err := keystore.SetFor(a, cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
			return err
		}
		exp := harness.Exporter{
			Endpoint:           cfg.OTLPURL,
			APIKey:             key,
			Signals:            signals,
			ProjectID:          cfg.ProjectID,
			ResourceAttributes: resourceAttributes(ctx, h, cfg, f.identity),
			IncludePrompts:     !f.excludePrompts,
			IncludeToolContent: !f.excludeToolContent,
		}
		switch a {
		case shim.AgentCodex:
			rec.ResourceAttributes = exp.ResourceAttributes
			rec.Harnesses = append(rec.Harnesses, a)
			if slices.Contains(agents, shim.AgentCodex) {
				fmt.Fprintln(out, "  Codex CLI    → per-repo runtime overrides")
			} else {
				fmt.Fprintln(out, "  Codex Desktop → per-repo logs route")
			}
			if slices.Contains(agents, codexDesktopAgent) {
				ui.ok("Codex Desktop", "reports through this repository's hooks")
			}
		case shim.AgentClaude:
			if _, err := shim.PrepareClaudeSettings(exp); err != nil {
				return fmt.Errorf("claude: %w", err)
			}
			rec.Harnesses = append(rec.Harnesses, a)
			fmt.Fprintln(out, "  Claude Code  → per-repo settings")
		case "opencode":
			// OpenCode routes itself: its plugin reads the repository's binding and picks
			// the project's key, so it needs no wrapper or shim.
			if err := (harness.OpenCode{}).ConnectPerRepo(exp); err != nil {
				return fmt.Errorf("opencode: %w", err)
			}
			// The plugin is global and shared across every bound repository, so a
			// per-repo prompt / tool-content choice cannot ride in it (that would flip
			// capture on for every other project). It lives only in a committed
			// .opencode/terma.json overlay, written by install below.
			fmt.Fprintln(out, "  OpenCode     → per-repo plugin")
			ui.ok("OpenCode", "reports through terma's plugin")
		default:
			// Every telemetry harness is routed above. One added to the registry without
			// a case here must not pass for routed: it would export to whatever project
			// the machine-wide connect last named.
			return fmt.Errorf("%s: no per-repository routing", a)
		}
	}

	if len(rec.Harnesses) == 0 {
		return nil
	}
	if err := shim.SaveRecord(rec); err != nil {
		return err
	}
	var launchedFromShell []string
	for _, name := range rec.Harnesses {
		if slices.Contains(agents, name) {
			launchedFromShell = append(launchedFromShell, name)
		}
	}
	if len(launchedFromShell) == 0 {
		return nil
	}
	err = setupActivation(cmd, ui, launchedFromShell, f)
	return err
}

// exportingAgents are the agents among a developer's that terma points at a project —
// the ones with an exporter, Codex Desktop counted as Codex.
func exportingAgents(agents []string) []string {
	var names []string
	for _, a := range telemetryAgentNames(agents) {
		if _, err := harness.Lookup(a); err == nil {
			names = append(names, a)
		}
	}
	return names
}

// setupActivation installs the per-repo routing mechanism.
// The PATH shim is the default and is installed without asking; the shim scripts front
// the agent binaries and re-invoke terma. `--activation wrapper` opts into printed shell
// functions instead. Unless the shims already route, the shim directory then goes on
// PATH in the developer's shell startup file (putShimsOnPath).
func setupActivation(cmd *cobra.Command, ui *installUI, agents []string, f installFlags) error {
	mode := strings.TrimSpace(f.activation)
	if mode == "" {
		mode = "shim"
	}
	switch mode {
	case "shim":
		binDir, err := shim.InstallShims(agents)
		if err != nil {
			return err
		}
		fmt.Fprintf(ui.detail, "  Routing      → PATH shims for %s in %s\n", joinNames(adapterDisplayNames(agents)), binDir)
		for _, a := range agents {
			ui.ok(adapterDisplayNames([]string{a})[0], "shim at "+tildePath(filepath.Join(binDir, a)))
		}
		if !allActive(agents) {
			putShimsOnPath(ui, binDir, agents, f)
		}
	case "wrapper":
		if _, err := shim.InstallShims(agents); err != nil {
			return err
		}
		for _, a := range agents {
			ui.ok(adapterDisplayNames([]string{a})[0], "routed by a shell function")
		}
		if !allActive(agents) {
			shell := filepath.Base(os.Getenv("SHELL"))
			file := wrapperFile(shell)
			ui.reloading = true
			ui.then(fmt.Sprintf("Add these to your %s so the agents route to this repo's project, then run `%s` in this terminal:\n%s",
				file, reloadCommand(file), ui.code(shim.WrapperSnippetFor(shell, agents))))
		}
	default:
		return fmt.Errorf("unknown --activation %q (want shim or wrapper)", mode)
	}
	return nil
}

// wrapperFile is the startup file the wrapper hint names: the one the developer's shell
// reads functions from. zsh and bash read the file the PATH block goes in (ShellRC); fish
// reads config.fish, never terma's own conf.d file; any other shell (sh, dash, BusyBox ash)
// is a POSIX shell whose login shells read ~/.profile.
func wrapperFile(shell string) string {
	if rc, ok := shim.ShellRC(); ok && rc.Shell == shell && shell != "fish" {
		return tildePath(rc.Path)
	}
	// Without a home directory each shell's usual file, literally: a path joined onto ""
	// would be relative, and ~/.profile is not what zsh or bash reads.
	switch shell {
	case "zsh":
		return "~/.zshrc"
	case "bash":
		return "~/.bashrc"
	case "fish":
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "~/.config/fish/config.fish"
		}
		return tildePath(filepath.Join(shim.FishConfigDir(home), "config.fish"))
	}
	return "~/.profile"
}

// putShimsOnPath gets the shim directory onto PATH ahead of the real binaries — the one
// step of per-repo routing that lives in the developer's shell startup file. It writes
// one marked block at the end of the file without asking: running install is the
// consent, and --no-path keeps terma out of the file and prints the line instead. The
// shell install was run from read that file before the block was in it, and terma, a
// child process, cannot change its parent's PATH — so whatever happens, a next step says
// how to make this shell read the file again.
//
// The block goes last on purpose. A PATH line only wins over the ones that run after it,
// and a real startup file prepends ~/.local/bin — where the real claude and codex live —
// more than once; pasted anywhere but the end, terma's line is silently overtaken.
func putShimsOnPath(ui *installUI, binDir string, agents []string, f installFlags) {
	ui.reloading = true
	names := joinNames(adapterDisplayNames(agents))
	rc, ok := shim.ShellRC()
	file := "your shell's startup file"
	if ok {
		file = tildePath(rc.Path)
	}
	manual := func(why string) {
		line := `export PATH="` + binDir + `:$PATH"`
		reload := "then start a new shell"
		if ok {
			line = rc.PathLine(binDir)
			reload = "then run `" + reloadCommand(file) + "` in this terminal"
		}
		ui.warn("PATH", "the shims are not on PATH yet")
		ui.then(fmt.Sprintf("%s as the LAST line that touches PATH in %s — a later line that prepends another directory puts the real binaries back in front:\n%s\n%s to route %s through terma.",
			why, file, ui.code(line), reload, names))
	}
	reload := fmt.Sprintf("Run `%s` in this terminal, or open a new terminal, to route %s through terma.", reloadCommand(file), names)
	if !ok || f.noPath {
		manual("Add this")
		return
	}
	state, err := rc.State()
	if err != nil {
		manual("Could not read " + file + " (" + err.Error() + "). Add this")
		return
	}
	if state == shim.RCLast {
		ui.ok("PATH", file+" puts the shims first")
		ui.then(reload)
		return
	}
	// Absent, or overtaken by a later line that puts the real binaries back in front:
	// either way Ensure appends the block, or moves it to the end.
	if _, err := rc.Ensure(binDir); err != nil {
		manual("Could not write " + file + " (" + err.Error() + "). Add this")
		return
	}
	fmt.Fprintf(ui.detail, "  PATH         → %s (last line; `terma shim uninstall` removes it).\n", file)
	if state == shim.RCOvertaken {
		ui.ok("PATH", "moved terma's line to the end of "+file+", so the shims come first")
	} else {
		ui.ok("PATH", file+" puts the shims first")
	}
	ui.then(reload)
}

// reloadCommand re-reads a startup file in the running shell: `source` where the shell
// has it (zsh, bash, fish), the POSIX `.` otherwise.
func reloadCommand(file string) string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh", "bash", "fish":
		return "source " + file
	}
	return ". " + file
}

// allActive reports whether every routed agent already resolves to terma's shim.
func allActive(agents []string) bool {
	for _, a := range agents {
		if shim.Routable(a) && !shim.Active(a) {
			return false
		}
	}
	return true
}

// installAdapters lists the agents whose committed hooks to wire. --adapters overrides
// it outright; otherwise it is the union of the agents the repository's hooks files
// already wire, the agents this install configures, and any adapter whose directory the
// repository already carries (a .codex directory is a clear sign the repo is opened in
// Codex) — restricted to adapters that actually write a hooks file, and to agents that
// are available: one still coming soon (agentAvailable — Cursor, Antigravity) is wired
// only when --adapters names it, whatever directory the repository carries. Hooks a
// colleague committed for one are left as they are, not rewritten or removed.
//
// A union (rather than replacing with the current selection) means selecting more agents
// grows the committed set, while a colleague re-running install with a narrower selection
// never removes hooks someone else committed — so the files grow on purpose and never
// churn down.
func installAdapters(root string, agents []string, override string) []string {
	if list := splitCommas(override); len(list) > 0 {
		return list
	}
	var out []string
	for _, a := range adapter.All() {
		if a.HooksPath() == "" || !agentAvailable(a.Name()) {
			continue
		}
		// Codex Desktop is captured through the Codex hooks file.
		selected := slices.Contains(agents, a.Name()) || (a.Name() == shim.AgentCodex && slices.Contains(agents, codexDesktopAgent))
		if selected || a.Default(root) || adapter.Wired(root, a) {
			out = append(out, a.Name())
		}
	}
	return out
}

// hookPlan is everything install would write into the repository for hooks: the
// commit hooks through the detected manager, and each wired agent's own hooks file.
type hookPlan struct {
	det    hookmgr.Detection
	hooks  hookmgr.Plan
	agents []hookmgr.Plan
}

func planHooks(root string, det hookmgr.Detection, adapters []string) (hookPlan, error) {
	var hooks hookmgr.Plan
	var err error
	if det.Manager != "" {
		hooks, err = hookmgr.PlanInstall(root, det)
		if err != nil {
			return hookPlan{}, err
		}
	}
	agents, err := planAdapters(root, adapters, true)
	if err != nil {
		return hookPlan{}, err
	}
	if err := hookmgr.Validate(root, hooks); err != nil {
		return hookPlan{}, err
	}
	return hookPlan{det: det, hooks: hooks, agents: agents}, nil
}

// empty reports whether the commit hooks and every agent's hooks are already in place.
func (p hookPlan) empty() bool {
	if !p.hooks.Empty() {
		return false
	}
	for _, a := range p.agents {
		if !a.Empty() {
			return false
		}
	}
	return true
}

// print lists the files the hook install would write, or says there are none.
func (p hookPlan) print(out io.Writer) {
	if p.empty() {
		fmt.Fprintln(out, "\nHooks already present — nothing to write.")
		return
	}
	if p.det.Manager == "" {
		fmt.Fprintln(out, "\nAgent hooks:")
	} else {
		fmt.Fprintf(out, "\nHooks — commit stamping via %s (%s):\n", p.det.Manager, p.det.Detail)
	}
	for _, c := range p.hooks.Changes {
		fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
	}
	for _, a := range p.agents {
		for _, c := range a.Changes {
			fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
		}
	}
	// What the hook manager needs from each clone (`lefthook install`, `pre-commit
	// install …`, husky's prepare script). Without these lines the hooks are committed
	// and never run, and nothing says why.
	if len(p.hooks.Notes) > 0 {
		fmt.Fprintln(out, "\nAfter merging:")
		for _, n := range p.hooks.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
}

// files names what the plan writes for a question that fits on a line: terma's own hook
// shims as their directory, every other file by its path.
func (p hookPlan) files() []string {
	var files []string
	for _, path := range p.paths() {
		if path = shownPath(path); !slices.Contains(files, path) {
			files = append(files, path)
		}
	}
	return files
}

// shownPath is how a question names a file the plan writes: terma's hook shims by their
// directory, every other file by its path.
func shownPath(path string) string {
	if strings.HasPrefix(path, hookmgr.ShimDir+"/") {
		return hookmgr.ShimDir + "/"
	}
	return path
}

// explain says what each file in files() is for, a line apiece, and what committing them
// means — the lines under the question that asks to write them, so a developer knows what
// a yes does before giving it.
func (p hookPlan) explain() []string {
	what := map[string]string{}
	for _, c := range p.hooks.Changes {
		what[shownPath(c.Path)] = "stamps each commit with the agent session that wrote it"
	}
	for _, a := range adapter.All() {
		if path := a.HooksPath(); path != "" {
			what[path] = "reports each " + a.DisplayName() + " session and the files it edits"
		}
	}
	files := p.files()
	width := 0
	for _, f := range files {
		width = max(width, len(f))
	}
	lines := make([]string, 0, len(files)+2)
	for _, f := range files {
		desc, ok := what[f]
		if !ok {
			desc = "terma's hook wiring"
		}
		lines = append(lines, fmt.Sprintf("%-*s  %s", width, f, desc))
	}
	return append(lines,
		"These are committed: merging them sets up everyone who clones the repository,",
		"and on a machine without terma they do nothing.")
}

// summary says what the hooks do once installed: commit stamping through the hook
// manager, and the agents whose own hooks report their sessions.
func (p hookPlan) summary(adapters []string) string {
	var parts []string
	if p.det.Manager != "" {
		parts = append(parts, "commit stamping via "+string(p.det.Manager))
	}
	var agents []string
	for _, name := range adapters {
		if a, ok := adapter.Lookup(name); ok {
			agents = append(agents, a.DisplayName())
		}
	}
	if len(agents) > 0 {
		parts = append(parts, "session hooks for "+joinNames(agents))
	}
	if len(parts) == 0 {
		return "none to write"
	}
	return strings.Join(parts, "; ")
}

// paths lists the files the plan writes or deletes, relative to the root, in the order
// print shows them.
func (p hookPlan) paths() []string {
	var paths []string
	for _, c := range p.hooks.Changes {
		paths = append(paths, c.Path)
	}
	for _, a := range p.agents {
		for _, c := range a.Changes {
			paths = append(paths, c.Path)
		}
	}
	return paths
}

// printCommitList tells the developer which files the hook install wrote and that they
// must be committed: the hooks do nothing for a colleague until the files are merged.
// lead is the sentence that says why. A path is listed once, even when two changes
// touched it.
func printCommitList(out io.Writer, lead string, paths []string) {
	fmt.Fprintln(out, commitList(style.For(out), lead, paths))
}

func (p hookPlan) apply(root string) error {
	if err := hookmgr.Apply(root, p.hooks); err != nil {
		return err
	}
	for _, a := range p.agents {
		if err := hookmgr.Apply(root, a); err != nil {
			return err
		}
	}
	return nil
}

// confirmYes prompts, detail under the question saying what a yes does, and treats a
// read error as "no".
func confirmYes(cmd *cobra.Command, question string, detail []string) bool {
	ok, err := confirmExplained(cmd, question, detail, true)
	return err == nil && ok
}

func managerOrEmpty(det hookmgr.Detection, installed bool) string {
	if !installed {
		return ""
	}
	return string(det.Manager)
}

func hooksOrNil(installed bool) []string {
	if !installed {
		return nil
	}
	return hookmgr.GitHooks
}

func signalStrings(signals []harness.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, string(s))
	}
	return out
}

// binding is the project a repository is tied to, and the environment it was chosen in.
type binding struct {
	ID, Name, OrganizationID, Environment string
}

// keptBinding is the repository's binding as it stands, environment included: a
// colleague confirming the project must not rewrite the committed file to match their
// own setup.
func keptBinding(existing *termaproject.File) binding {
	p := existing.Project
	return binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: p.Environment}
}

// boundTo is a newly chosen project, recorded with the environment it was chosen in.
func boundTo(p *project, cfg *config.Config) binding {
	return binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: nonProd(cfg.Environment)}
}

// resolveBinding picks the project: an explicit reference (matched against the
// organization's projects, falling back to a picker when it does not match), else the
// organization's projects with the existing binding offered first.
//
// verify says install signs in to act for the project, so the binding is checked against
// the projects that credential can see: one made in another environment or organization
// names a project the account service refuses, and used as-is it failed at the first key
// install minted, with the server's words about neither. ask says a person is there to
// choose: they confirm or change the project on every install, the bound one marked and
// kept by Enter — unless the organization has one project, which is taken without asking.
// Without ask, a binding that checks out is kept and one that does not is an error naming
// the fix. A hooks-only install that needs no credential keeps the binding unchecked —
// there is nothing to check it with.
func resolveBinding(cmd *cobra.Command, cfg *config.Config, existing *termaproject.File, ref string, verify, ask bool) (binding, error) {
	ref = strings.TrimSpace(ref)
	if serverkey.Is(cfg.APIKey) {
		return serverKeyBinding(cmd.Context(), cfg, existing, ref)
	}
	current := ""
	if existing != nil {
		current = existing.Project.ID
	}
	if ref != "" {
		client, err := newClient(cfg)
		if err != nil {
			if errors.Is(err, auth.ErrNotLoggedIn) {
				return binding{ID: ref, Environment: nonProd(cfg.Environment)}, nil
			}
			return binding{}, err
		}
		projects, err := fetchProjects(cmd.Context(), client)
		if err != nil {
			return binding{}, err
		}
		if p, err := matchProject(projects, ref); err == nil {
			return boundTo(p, cfg), nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "No project matches %q in this organization — pick one:\n", ref)
		p, err := pickProject(cmd, projects, current)
		if err != nil {
			return binding{}, err
		}
		return boundTo(p, cfg), nil
	}
	if existing != nil && !verify {
		return keptBinding(existing), nil
	}

	client, err := newClient(cfg)
	if err != nil {
		// A dry run does not sign in; without a credential the binding stands unchecked.
		if existing != nil && errors.Is(err, auth.ErrNotLoggedIn) {
			return keptBinding(existing), nil
		}
		return binding{}, err
	}
	projects, err := availableProjects(cmd.Context(), client)
	if errors.Is(err, errNoProjects) {
		return binding{}, fmt.Errorf("%w, then run `terma install` again", err)
	}
	if err != nil {
		return binding{}, err
	}
	bound := existing != nil && slices.ContainsFunc(projects, func(p project) bool { return p.ID == current })

	switch {
	case existing != nil && !bound && !ask:
		return binding{}, fmt.Errorf("%s — run `terma install --project <name or id>` with one of yours (`terma project list` lists them)", unreachableBinding(existing, cfg))
	case existing != nil && !bound:
		reason := unreachableBinding(existing, cfg)
		fmt.Fprintf(cmd.ErrOrStderr(), "%s%s.\n", strings.ToUpper(reason[:1]), reason[1:])
		current = ""
	case bound && !ask:
		return keptBinding(existing), nil
	}

	// One project is no choice, so it is taken without asking — on a first install and in
	// place of a binding the account cannot see alike. Several are a picker, the bound one
	// marked and kept by Enter.
	p, err := soleOrPick(cmd, projects, current)
	if err != nil {
		return binding{}, err
	}
	if bound && p.ID == current {
		return keptBinding(existing), nil
	}
	return boundTo(p, cfg), nil
}

// unreachableBinding says why the repository's project is not among those the current
// credential can see: the environment it was bound in when that differs, else the
// organization it belongs to, which the developer may be able to switch to.
func unreachableBinding(existing *termaproject.File, cfg *config.Config) string {
	p := existing.Project
	org := nameOrID(cfg.OrganizationName, cfg.OrganizationID)
	if org == "" {
		org = "your organization"
	}
	msg := fmt.Sprintf("this repository is bound to %s, which is not a project in %s", nameOrID(p.Name, p.ID), org)
	switch {
	case !config.SameAccounts(p.Environment, cfg.Environment):
		return fmt.Sprintf("%s: it was bound in %s, and terma is using %s", msg, environmentLabel(p.Environment), environmentLabel(cfg.Environment))
	case p.OrganizationID != "" && p.OrganizationID != cfg.OrganizationID:
		return fmt.Sprintf("%s: it belongs to organization %s (`terma org use %s` switches to it, if you are a member)", msg, p.OrganizationID, p.OrganizationID)
	}
	return msg
}

// environmentLabel names a built-in environment in a sentence.
func environmentLabel(env string) string {
	if env == "" || env == config.EnvProd {
		return "production"
	}
	return "the " + env + " environment"
}

// serverKeyBinding is the project a server key (TERMA_API_KEY) belongs to. A ter_srv_ key
// is scoped to exactly one project, and the account service that lists projects accepts
// only a signed-in user, so the API gateway's /v1/identity is what can say which project
// it is. A --project or an existing binding that names a different project is an error
// rather than a silent switch: the key could not deliver that project's events. It asks
// on every install, a reinstall with a binding included, because that is what verifies the
// key still belongs to the bound project; the key needs the network to deliver anyway.
// /v1/identity names no project, so a first install records none (Name is omitempty) and
// output falls back to the id.
func serverKeyBinding(ctx context.Context, cfg *config.Config, existing *termaproject.File, ref string) (binding, error) {
	client, err := newClient(cfg)
	if err != nil {
		return binding{}, err
	}
	var identity struct {
		ProjectID      string `json:"project_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := client.Get(ctx, "/v1/identity", nil, &identity); err != nil {
		return binding{}, fmt.Errorf("look up the project TERMA_API_KEY belongs to: %w", err)
	}
	if identity.ProjectID == "" {
		return binding{}, errors.New("TERMA_API_KEY names no project")
	}
	if ref != "" && ref != identity.ProjectID {
		return binding{}, fmt.Errorf("TERMA_API_KEY belongs to project %s, not %q — a server key binds only its own project, named by id", identity.ProjectID, ref)
	}
	b := binding{ID: identity.ProjectID, OrganizationID: identity.OrganizationID, Environment: nonProd(cfg.Environment)}
	if existing != nil {
		if existing.Project.ID != identity.ProjectID {
			return binding{}, fmt.Errorf("this repository is bound to project %s, and TERMA_API_KEY belongs to %s", existing.Project.ID, identity.ProjectID)
		}
		b.Name = existing.Project.Name
	}
	return b, nil
}

// planAdapters computes each named adapter's repository changes, in registry order so
// the plan reads the same way every time whatever order --adapters listed them in.
func planAdapters(root string, names []string, install bool) ([]hookmgr.Plan, error) {
	want := map[string]bool{}
	for _, name := range names {
		if _, ok := adapter.Lookup(name); !ok {
			return nil, fmt.Errorf("unknown agent %q (want %s)", name, joinNames(adapter.RepoNames()))
		}
		want[name] = true
	}
	var plans []hookmgr.Plan
	for _, a := range adapter.All() {
		if !want[a.Name()] {
			continue
		}
		p, err := a.Plan(root, install)
		if err != nil {
			return nil, err
		}
		if err := hookmgr.Validate(root, p); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

func nonProd(env string) string {
	if env == config.EnvProd {
		return ""
	}
	return env
}

// splitCommas splits a comma-joined list — a flag's value, the `sessions` attribute of
// a commit record — dropping empties and the spaces around each item.
func splitCommas(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// --- uninstall ---------------------------------------------------------------------

func newUninstallCommand() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything terma install wrote to this repository",
		Long: `Symmetric with install: removes terma's hook wiring (only terma's lines from
shared hook files), its agent hooks (Claude Code, Cursor, Codex, Antigravity),
.terma/settings.json, the per-clone git configuration, and the local session state.
Removing the binding un-routes this checkout; the home-dir routing state (keys, routing
records) is kept, since it is shared with any other worktree or clone bound
to the same project — remove it machine-wide with 'terma shim uninstall'.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			root, gitDir, err := workspaceHere(ctx)
			if err != nil {
				return err
			}
			for _, path := range []string{termaproject.FileName, hookmgr.ClaudeSettingsPath} {
				if err := termaproject.CheckPath(root, path); err != nil {
					return err
				}
			}
			existing, err := termaproject.Load(root)
			if err != nil && !errors.Is(err, termaproject.ErrNotFound) {
				return err
			}
			det := hookmgr.Detect(root)
			if existing != nil && existing.Install.HookManager != "" {
				det.Manager = hookmgr.Manager(existing.Install.HookManager)
				if det.Manager == hookmgr.GitShim {
					det.ConfigPath = hookmgr.ShimDir
				}
			}
			var hooks hookmgr.Plan
			if gitDir != "" {
				hooks, err = hookmgr.PlanUninstall(root, det)
				if err != nil {
					return err
				}
			}
			if err := hookmgr.Validate(root, hooks); err != nil {
				return err
			}
			plans, err := planAdapters(root, adapter.Names(), false)
			if err != nil {
				return err
			}
			changes := append([]hookmgr.Change{}, hooks.Changes...)
			for _, p := range plans {
				changes = append(changes, p.Changes...)
			}
			for _, note := range hooks.Notes {
				fmt.Fprintln(out, note)
			}
			if len(changes) == 0 && existing == nil {
				fmt.Fprintln(out, "Nothing of terma's is installed here.")
				return nil
			}
			fmt.Fprintln(out, "Files:")
			for _, c := range changes {
				fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
			}
			if existing != nil {
				fmt.Fprintf(out, "  %-7s %s\n", "delete", termaproject.FileName)
			}
			if gitDir != "" && gitx.ConfigGet(ctx, root, "core.hooksPath") == hookmgr.ShimDir {
				fmt.Fprintln(out, "  restore git config core.hooksPath")
			}
			if !assumeYes {
				ok, err := confirm(cmd, "Remove these?")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "Cancelled. Nothing was removed.")
					return nil
				}
			}
			if err := hookmgr.Apply(root, hooks); err != nil {
				return err
			}
			for _, p := range plans {
				if err := hookmgr.Apply(root, p); err != nil {
					return err
				}
			}
			// Every harness that reads a repository policy, not only the ones some
			// install chose: uninstall removes whatever of terma's is here, and a policy
			// that is not there is nothing to remove.
			for _, h := range harness.All() {
				scoped, ok := h.(harness.Scoped)
				if !ok {
					continue
				}
				if _, err := scoped.Local(root).Disconnect(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not remove %s's repository policy: %v\n", h.DisplayName(), err)
				}
			}
			// Per-repo routing state (the routing record) is
			// keyed by project id, not by clone, and shared across every worktree or
			// repository bound to that project — so it is left in place, exactly as the
			// keystore keys are. Removing .terma/settings.json below un-binds this checkout,
			// which is what stops routing here; other checkouts of the same project keep
			// working. `terma shim uninstall` is the machine-level teardown.
			if gitDir != "" {
				if err := unwireRepo(ctx, root, gitDir); err != nil {
					return err
				}
			}
			if err := termaproject.Remove(root); err != nil {
				return err
			}
			stateDir, err := termaproject.StateDir(root, gitDir)
			if err != nil {
				return err
			}
			if err := session.Open(stateDir).Remove(); err != nil {
				return err
			}
			if stateDir != gitDir {
				// A workspace that gained Git retains private session storage, but
				// its Git hook restoration journal lives in the worktree metadata.
				if gitDir != "" {
					if err := session.Open(gitDir).Remove(); err != nil {
						return err
					}
				}
				// Remove the reservation only when empty; leave any unrelated files.
				_ = os.Remove(stateDir)
			}
			if gitDir != "" {
				fmt.Fprintln(out, "Uninstalled. Commit the removals if the install was committed.")
			} else {
				fmt.Fprintln(out, "Uninstalled.")
			}
			fmt.Fprintln(out, "Per-repo routing (a PATH shim and routing records) stays on your machine — run `terma shim uninstall` when you no longer route any repo.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
