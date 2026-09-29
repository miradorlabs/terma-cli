package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	"github.com/miradorlabs/terma-cli/internal/spinner"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type installFlags struct {
	projectRef   string
	harnesses    string
	adapters     string
	noHooks      bool
	noDoctor     bool
	noStatusLine bool
	// The export choices are machine-wide (`terma setup` records them); given here, they
	// update that record and rewrite the agents' global configuration.
	identity string
	signals  string
	// prompts is --prompts: "on", "off", or "" (keep this machine's choice).
	prompts            string
	excludePrompts     bool
	excludeToolContent bool
	noBrowser          bool
	dryRun             bool
	assumeYes          bool
	force              bool
	// verbose prints setup steps and their details.
	verbose bool
	// Accepted and ignored: per-repository routing (PATH shims, shell wrappers) is gone,
	// and a script that still passes them must not start failing.
	activation string
	noPath     bool
	// noRelay is setup's --no-relay, for a script that installs without running setup.
	noRelay bool
}

func newInstallCommand() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Configure this workspace: bind it to its project and wire the hooks",
		Long: `Run once per repository. install is self-contained — it signs you in if you have
not run ` + "`terma setup`" + `, asks which agents you use if you have not chosen, then:

  1. Binds the repository to a Terma project and records it in .terma/settings.json —
     committed, no secrets. An organization with one project is bound to it without
     asking. With several, on a terminal you choose the project every time, the one
     already bound offered first (Enter keeps it); --project names it instead, and
     without a terminal, or with --yes, an existing binding is kept. A binding to a
     project your account cannot see is not used: install says why and chooses again.
  2. Makes sure your agents' telemetry is configured. That is machine-wide — one
     global configuration per agent, written by ` + "`terma setup`" + ` — and terma's relay
     sends each session to the project of the repository it ran in, this one included.
     When setup has not configured it yet, install does, with this repository's
     project as the machine's.
  3. Offers to install the commit hooks and the agents' own hooks (session start/end,
     tool use, stop) into the files you commit, so one merged PR onboards everyone.

Run from any subdirectory of a Git worktree; install uses its root. Outside Git,
the first install uses the current directory and later installs find the nearest
.terma/settings.json above it. Agent hooks and telemetry work there; Git hooks and
commit stamping are skipped. Recognizable non-Git repositories produce a warning:
only Git has version-control integration. Bare repositories are not workspaces.

Keys live in your home directory; the committed .terma/settings.json only names the
project.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.projectRef, "project", "", "Terma project (name or id) to bind the repository to")
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to configure ("+strings.Join(availableAgentNames(), ", ")+"); default: what `terma setup` recorded, else a picker")
	cmd.Flags().StringVar(&f.adapters, "adapters", "", "comma-separated agents whose committed hooks to wire (default: the configured agents that have one)")
	cmd.Flags().BoolVar(&f.noHooks, "no-hooks", false, "do not install commit hooks or agent hooks")
	cmd.Flags().BoolVar(&f.noDoctor, "no-doctor", false, "do not run `terma doctor` to verify the chain after installing")
	cmd.Flags().BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap Claude Code's status line (which captures the plan's rate-limit windows)")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "show what would change without writing anything")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "show setup steps and what each step wrote")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace another collector's settings in an agent's global configuration")
	// Machine-wide export choices, `terma setup`'s to make; kept here for the scripts
	// that pass them, where they update the machine's record.
	cmd.Flags().StringVar(&f.identity, "identity", "", "identity stamped on Codex sessions (default: git user.email; \"none\" to omit)")
	cmd.Flags().StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
	cmd.Flags().StringVar(&f.prompts, "prompts", "", "send prompt text and model responses: on or off (default: this machine's choice)")
	cmd.Flags().BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	cmd.Flags().BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	cmd.Flags().BoolVar(&f.noRelay, "no-relay", false, "export straight to Terma instead of through terma's relay (a machine-wide choice; see `terma setup`)")
	cmd.Flags().StringVar(&f.activation, "activation", "", "no longer used")
	cmd.Flags().BoolVar(&f.noPath, "no-path", false, "no longer used")
	for _, name := range []string{"identity", "signals", "prompts", "exclude-prompts", "exclude-tool-content", "no-relay", "activation", "no-path"} {
		_ = cmd.Flags().MarkHidden(name)
	}
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
	// The machine-wide export choices, with any this run's flags change.
	t := cfg.Telemetry
	exportChanged, err := applyExportFlags(cmd, &t, f)
	if err != nil {
		return err
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
		ui.summary("Project", nameOrID(b.Name, b.ID)+env)
	}

	// Whether the developer's agents send what was said is a machine-wide choice, and
	// nothing asks here, so the line names the command that changes it.
	if len(machineAgents(agents)) > 0 {
		if !t.ExcludePrompts {
			ui.summary("Prompts", "prompt text and model responses are sent — `terma setup --prompts off` stops them")
		} else {
			ui.summary("Prompts", "prompt text and model responses are not sent — `terma setup --prompts on` sends them")
		}
	}

	// The hook plan — the commit hooks and the agents' own hooks, into committed files —
	// is built once, before anything is written, so a dry run prints exactly the plan an
	// install goes on to apply. The wired adapters are a team decision, so a re-install
	// keeps every agent the repository's hooks files already wire unless --adapters
	// overrides it — a colleague re-running install must not rewrite the committed hooks
	// to match their own agent set.
	adapters := installAdapters(root, agents, f.adapters)
	det := hookmgr.Detect(root)
	if gitDir == "" {
		det = hookmgr.Detection{}
	}
	var plan hookPlan
	if !f.noHooks {
		if plan, err = planHooks(root, det, adapters); err != nil {
			return err
		}
	}

	if f.dryRun {
		if !f.noHooks {
			plan.print(out)
		}
		if needsAuth {
			fmt.Fprintln(out, "\nA real install would sign in first (not done for a dry run).")
		}
		if len(machineAgents(agents)) > 0 && (exportChanged || !machineTelemetryCurrent(cfg, agents)) {
			fmt.Fprintf(out, "\nTelemetry: a real install configures %s machine-wide (their global settings files), through terma's relay where it can run.\n",
				joinNames(harnessDisplayNames(machineAgents(agents))))
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

	// 4. The agents' telemetry, machine-wide. Configured when setup has not done it, when
	// an agent this run selects is not pointed where the machine's mode says, or when this
	// run's flags change an export choice. Otherwise the relay already sends this
	// repository's sessions to its project and there is nothing to write.
	if len(machineAgents(agents)) > 0 {
		if exportChanged || !machineTelemetryCurrent(cfg, agents) {
			if t.Project.ID == "" {
				t.Project = config.ProjectRef{ID: b.ID, Name: b.Name, OrganizationID: b.OrganizationID}
			}
			sp := spinner.New(cmd.ErrOrStderr())
			sp.Start("Configuring your agents' telemetry…")
			res, err := configureMachineTelemetry(ctx, cmd.ErrOrStderr(), cfg, agents, t, machineOptions{force: f.force, noStatusLine: true})
			sp.Stop()
			if err != nil {
				return err
			}
			printMachineResult(ui, res)
		} else {
			ui.ok("Telemetry", "configured machine-wide")
		}
		if cfg.Telemetry.Mode == config.TelemetryDirect && cfg.Telemetry.Project.ID != b.ID {
			ui.warn("Telemetry", "this machine's agents export to "+nameOrID(cfg.Telemetry.Project.Name, cfg.Telemetry.Project.ID)+", not to this repository's project")
			ui.then("Run `terma setup` to send each session to its repository's project through terma's relay.")
		}
	}
	if slices.Contains(agents, "opencode") {
		if err := connectOpenCodeForRepo(ctx, ui, cfg, t); err != nil {
			return err
		}
	}
	if removed, err := cleanupLegacyRouting(); err != nil {
		ui.warn("Cleanup", "could not remove the old per-repository routing ("+err.Error()+")")
		ui.then("Run `terma shim uninstall` to remove it.")
	} else if removed {
		ui.ok("Cleanup", "removed the old per-repository routing (PATH shims, routing records)")
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
		writeHooks := f.assumeYes
		if !plan.empty() && !writeHooks {
			var err error
			writeHooks, err = confirmExplained(cmd, "Write terma's hooks to "+joinNames(plan.files())+"?", plan.explain(), true)
			if errors.Is(err, errCancelled) {
				return err
			}
			writeHooks = err == nil && writeHooks
		}
		switch {
		case plan.empty():
			// An empty git-hook plan means the commit hooks are already wired, so this
			// repo is hook-installed; record them in the binding without rewriting.
			installedHooks = gitDir != ""
			ui.ok("Hooks", plan.summary(adapters)+" — already in place")
		case writeHooks:
			if err := plan.apply(root); err != nil {
				return err
			}
			installedHooks = gitDir != ""
			written = plan.paths()
			ui.ok("Hooks", plan.summary(adapters))
			afterMerge = plan.hooks.Notes
		default:
			ui.warn("Hooks", "not written — commits are not stamped until they are")
			ui.then("Run `terma install` again and accept the hooks when you are ready.")
		}
	}

	// The key this machine delivers the repository's hook events with. Pointing a
	// telemetry agent stores one as a side effect; nothing else does, and `terma setup`
	// no longer connects anything — so without this step a developer whose agents are
	// all hooks-only would see "Installed." while every commit, tool call and observation
	// sat in the spool, held for a key that no command would ever mint.
	if installedHooks || len(adapter.WiredNames(root)) > 0 {
		sp := spinner.New(cmd.ErrOrStderr())
		sp.Start("Preparing hook event delivery…")
		k := ensureSpoolKey(ctx, cfg)
		sp.Stop()
		if k.fix == "" {
			ui.ok("Hook events", k.state)
		} else {
			ui.warn("Hook events", k.state)
			ui.then(k.fix)
		}
	}

	// 6. A telemetry policy an earlier install committed would outrank the machine's
	// configuration in this repository, and switch it off.
	if path, err := stripRepoPolicy(root); err != nil {
		ui.warn("Telemetry", "could not read this repository's Claude Code settings ("+err.Error()+")")
	} else if path != "" {
		rel := gitx.Relativize(root, path)
		ui.ok("Telemetry", "removed the repository policy an earlier terma committed to "+rel)
		if !slices.Contains(written, rel) {
			written = append(written, rel)
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
		ui.then("Approve Terma's hooks in Codex Desktop:\n" +
			"a. Open this repository in Codex Desktop and trust the project if prompted.\n" +
			"b. Open Settings → Hooks, then select Review for the entries from .codex/hooks.json.\n" +
			"c. Inspect and approve each Terma hook command. Codex CLI is not required.\n" +
			"d. Run `terma desktop status` to confirm 'Codex hooks: ready'.")
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

// applyExportFlags folds this run's export flags into the machine's choices, and
// reports whether any changed one: an install that passes --prompts off means it for
// the machine, since the setting lives there now.
func applyExportFlags(cmd *cobra.Command, t *config.Telemetry, f installFlags) (bool, error) {
	before := *t
	flags := cmd.Flags()
	switch p := strings.ToLower(strings.TrimSpace(f.prompts)); p {
	case "on":
		if flags.Changed("exclude-prompts") && f.excludePrompts {
			return false, errors.New("--prompts on and --exclude-prompts disagree; pass one")
		}
		t.ExcludePrompts = false
	case "off":
		t.ExcludePrompts = true
	case "":
		if flags.Changed("exclude-prompts") {
			t.ExcludePrompts = f.excludePrompts
		}
	default:
		return false, fmt.Errorf("--prompts %q: want on or off", f.prompts)
	}
	if flags.Changed("signals") {
		if _, err := harness.ParseSignals(f.signals); err != nil {
			return false, err
		}
		t.Signals = f.signals
	}
	if flags.Changed("exclude-tool-content") {
		t.ExcludeToolContent = f.excludeToolContent
	}
	if flags.Changed("identity") {
		t.Identity = f.identity
	}
	if flags.Changed("no-relay") {
		t.NoRelay = f.noRelay
	}
	return *t != before, nil
}

// connectOpenCodeForRepo points OpenCode at the repository's project. OpenCode routes
// itself: its plugin reads the repository's binding and picks the project's key.
func connectOpenCodeForRepo(ctx context.Context, ui *installUI, cfg *config.Config, t config.Telemetry) error {
	h := harness.OpenCode{}
	key, _, _, _, err := resolveKey(ctx, cfg, h, connectFlags{})
	if err != nil {
		return fmt.Errorf("opencode: %w", err)
	}
	if err := keystore.SetFor(h.Name(), cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
		return err
	}
	signals, err := harness.ParseSignals(t.Signals)
	if err != nil {
		return err
	}
	exp := harness.Exporter{
		Endpoint:           cfg.OTLPURL,
		APIKey:             key,
		Signals:            signals,
		ProjectID:          cfg.ProjectID,
		ResourceAttributes: resourceAttributes(ctx, h, cfg, t.Identity),
		IncludePrompts:     !t.ExcludePrompts,
		IncludeToolContent: !t.ExcludeToolContent,
	}
	if err := h.ConnectPerRepo(exp); err != nil {
		return fmt.Errorf("opencode: %w", err)
	}
	ui.ok("OpenCode", "reports through terma's plugin")
	return nil
}

// harnessDisplayNames is each harness's name in prose.
func harnessDisplayNames(hs []harness.Harness) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.DisplayName())
	}
	return out
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
	sp := spinner.New(cmd.ErrOrStderr())
	sp.Start("Loading projects…")
	defer sp.Stop()
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
		sp.Stop()
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
	sp.Stop()
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
Removing the binding sends this checkout's sessions to the machine's project; the keys
and the agents' machine-wide telemetry configuration are kept, since they serve every
other repository — ` + "`terma disconnect <agent>`" + ` removes an agent's.`,
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
			// The keys and the agents' machine-wide configuration serve every repository,
			// so they stay. Removing .terma/settings.json below un-binds this checkout:
			// the relay sends its sessions to the machine's project from now on.
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
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
