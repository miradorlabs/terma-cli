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
	// developer to place instead of offering to write it.
	noPath             bool
	identity           string
	signals            string
	excludePrompts     bool
	excludeToolContent bool
	// telemetry keeps the legacy --telemetry=false opt-out working. Repository
	// policies are installed by default, including for older global connections.
	telemetry    bool
	updatePolicy bool
	noBrowser    bool
	dryRun       bool
	assumeYes    bool
	force        bool
}

func newInstallCommand() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Configure this repository: point your agents at its project and wire the hooks",
		Long: `Run once per repository. install is self-contained — it signs you in if you have
not run ` + "`terma setup`" + `, asks which agents you use if you have not chosen, then:

  1. Binds the repository to a Terma project (--project, an existing binding, or a
     picker) and records it in .terma/settings.json — committed, no secrets.
  2. Points each of your agents at that project, per repository:
       - Claude Code exports to it through per-repo settings (claude --settings);
       - Codex CLI exports to it through runtime -c overrides;
     both delivered by PATH shims installed automatically, which install offers to put
     on PATH in your shell's startup file (--no-path prints the line instead;
     --activation wrapper prints shell functions instead). Keys stay in your home directory, namespaced by project —
     never in the repository.
     Codex Desktop reports through repository hooks and the local Terma spool.
  3. Enables repository telemetry, including for machines configured to export only
     from installed repositories. Existing repository policies are preserved unless
     --signals or a content flag changes them.
  4. Offers to install the commit hooks and the agents' own hooks (session start/end,
     tool use, stop) into the files you commit, so one merged PR onboards everyone.

The keys and per-project configuration live in your home directory; the committed
.terma/settings.json only names the project.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.updatePolicy = cmd.Flags().Changed("signals") || cmd.Flags().Changed("exclude-prompts") || cmd.Flags().Changed("exclude-tool-content")
			return runInstall(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.projectRef, "project", "", "Terma project (name or id) to bind the repository to")
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to configure ("+strings.Join(availableAgentNames(), ", ")+"); default: what `terma setup` recorded, else a picker")
	cmd.Flags().StringVar(&f.adapters, "adapters", "", "comma-separated agents whose committed hooks to wire (default: the configured agents that have one)")
	cmd.Flags().StringVar(&f.activation, "activation", "", "per-repo routing delivery: shim (PATH shims, the default) or wrapper (printed shell functions)")
	cmd.Flags().BoolVar(&f.noHooks, "no-hooks", false, "do not install commit hooks or agent hooks")
	cmd.Flags().BoolVar(&f.noDoctor, "no-doctor", false, "do not run `terma doctor` to verify the chain after installing")
	cmd.Flags().BoolVar(&f.noPath, "no-path", false, "do not offer to add the shim directory to PATH in your shell's startup file; print the line instead")
	cmd.Flags().BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap Claude Code's status line (which captures the plan's rate-limit windows)")
	cmd.Flags().StringVar(&f.identity, "identity", "", "identity stamped on Codex/OpenCode sessions (default: git user.email; \"none\" to omit)")
	cmd.Flags().StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
	cmd.Flags().BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	cmd.Flags().BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	cmd.Flags().BoolVar(&f.telemetry, "telemetry", true, "write the repository telemetry policy (enabled by default)")
	_ = cmd.Flags().MarkHidden("telemetry")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "show what would change without writing anything")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace conflicting harness settings instead of refusing")
	return cmd
}

func runInstall(cmd *cobra.Command, f installFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	root, _, err := repoHere(ctx, "terma install runs inside a git repository")
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	existing, _ := termaproject.Load(root)

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
	b, err := resolveBinding(cmd, cfg, existing, f.projectRef)
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

	fmt.Fprintf(out, "Repository: %s\n", root)
	if b.ID == "" && b.Name == "" {
		fmt.Fprintln(out, "Project:    (unresolved — a real install signs in and selects one)")
	} else {
		fmt.Fprintf(out, "Project:    %s\n", nameOrID(b.Name, b.ID))
	}
	if cfg.Environment != config.EnvProd {
		fmt.Fprintf(out, "Environment: %s\n", cfg.Environment)
	}

	// The hook plan — the commit hooks and the agents' own hooks, into committed files —
	// is built once, before anything is written, so a dry run prints exactly the plan an
	// install goes on to apply. The wired adapters are a team decision, so a re-install
	// keeps whatever the binding already records unless --adapters overrides it — a
	// colleague re-running install must not rewrite the committed hooks to match their
	// own agent set.
	adapters := installAdapters(root, agents, f.adapters, existing)
	if slices.Contains(agents, codexDesktopAgent) && !slices.Contains(adapters, shim.AgentCodex) {
		return errors.New("codex desktop needs the Codex repository hooks; include codex in --adapters")
	}
	det := hookmgr.Detect(root)
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
		if f.telemetry {
			for _, h := range repoPolicyHarnesses(adapters) {
				path, err := h.(harness.Scoped).Local(root).ConfigPath()
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "\nRepository telemetry: %s (preserve existing policy unless export flags are supplied).\n", path)
			}
		}
		if slices.Contains(agents, codexDesktopAgent) {
			fmt.Fprintln(out, "\nCodex Desktop: write repository hooks and a local project route; review the hooks in Codex Desktop's Hooks settings before they can run. No Codex CLI is needed.")
		}
		fmt.Fprintln(out, "\nDry run: nothing written.")
		return nil
	}

	// 4. Configure each agent for this repository (per-repo routing). This is the
	// per-developer half — keys and routing state in the home directory — and touches
	// no committed file.
	if err := connectHarnessesForRepo(cmd, cfg, agents, f); err != nil {
		return err
	}

	// Wrap Claude Code's status line to capture the plan's rate-limit windows — the
	// strongest funding evidence a machine produces. It is machine-level (the user's
	// global Claude settings) and idempotent. Gated on Claude being a configured agent
	// (not merely installed) so it only touches the global config for a developer who
	// has chosen Claude — which also keeps `--harness none` installs from touching it.
	// --no-statusline opts out.
	if !f.noStatusLine && slices.Contains(agents, shim.AgentClaude) {
		if note := installStatusLine(cmd.ErrOrStderr()); note != "" {
			fmt.Fprintf(out, "\n%s\n", note)
		}
	}

	// 5. Hooks: apply the plan built above.
	installedHooks := existing != nil && existing.Install.HookManager != ""
	var written []string // the repository files this run wrote hooks into, to commit
	if !f.noHooks {
		plan.print(out)
		switch {
		case plan.empty():
			// An empty git-hook plan means the commit hooks are already wired, so this
			// repo is hook-installed; record them in the binding without rewriting.
			installedHooks = true
		case f.assumeYes || confirmYes(cmd, "Install these hooks?"):
			if err := plan.apply(root); err != nil {
				return err
			}
			installedHooks = true
			written = plan.paths()
		default:
			adapters = committedAdapters(existing) // declined: record only what is wired
		}
	} else {
		adapters = committedAdapters(existing) // --no-hooks: record only what is wired
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
	if installedHooks {
		if note := ensureSpoolKey(ctx, cfg); note != "" {
			fmt.Fprintf(out, "\n%s\n", note)
		}
	}

	// 6. Repository telemetry also supports developers using a global repos-only
	// connection. Hooks alone do not enable that connection's exporters.
	if f.telemetry {
		paths, err := writeRepoPolicy(ctx, out, root, cfg, repoPolicyHarnesses(adapters), f)
		if err != nil {
			return err
		}
		for _, path := range paths {
			if !slices.Contains(written, path) {
				written = append(written, path)
			}
		}
	}

	// 7. The committed binding, migrating any legacy .terma.toml in place. installed_at
	// and terma_version are preserved on a re-install so a colleague setting themselves
	// up does not churn the committed file — only the onboarder stamps them.
	version, installedAt := Version, time.Now().UTC()
	if existing != nil {
		if existing.Install.Version != "" {
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
			Environment:    nonProd(cfg.Environment),
		},
		Install: termaproject.Install{
			HookManager: managerOrEmpty(det, installedHooks),
			Hooks:       hooksOrNil(installedHooks),
			Adapters:    adapters,
			Version:     version,
			InstalledAt: installedAt,
		},
	}
	_, legacyErr := os.Stat(termaproject.LegacyPath(root))
	if err := termaproject.Save(root, file); err != nil {
		return err
	}

	// 8. Per-clone git wiring for the shim manager.
	if installedHooks {
		if err := wireRepo(ctx, out, root, file); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
		}
	}
	if slices.Contains(agents, codexDesktopAgent) {
		removed, err := removeLegacyDesktopRelay(cmd)
		if err != nil {
			return fmt.Errorf("remove previous Desktop relay: %w", err)
		}
		if removed {
			fmt.Fprintln(out, "Removed the previous Desktop relay; restart Codex Desktop to unload its old exporter.")
		}
		fmt.Fprintln(out, "Codex Desktop captures this repository through trusted hooks and Terma's existing spool.")
		fmt.Fprintln(out, "Open this repository in Codex Desktop and review Terma's hooks in Hooks settings before they can run. Codex CLI is not required.")
		if global, err := (harness.Codex{}).Status(); err == nil && global.Connected {
			fmt.Fprintln(out, "Warning: Codex also has a user-level exporter; it may send Desktop activity from other repositories.")
		}
	}

	fmt.Fprintf(out, "\n%s\n", style.For(out).Bold("Installed."))
	if len(written) > 0 {
		// Save always rewrites the binding, and removes a legacy .terma.toml it migrated.
		written = append(written, termaproject.FileName)
		if legacyErr == nil {
			written = append(written, termaproject.LegacyFileName)
		}
		printCommitList(out, written)
	}
	// Verify the chain right away. Skipped without a terminal (a script, CI) or with
	// --no-doctor, since doctor makes a scratch commit and a network round-trip; those
	// callers can run `terma doctor` themselves.
	if f.noDoctor || f.dryRun || !canPrompt() {
		fmt.Fprintln(out, "Run `terma doctor` to verify the chain end to end.")
		return nil
	}
	fmt.Fprintf(out, "\n%s\n", style.For(out).Bold("Verifying the chain (terma doctor):"))
	executeDoctor(cmd, false)
	return nil
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

// ensureSpoolKey makes sure this machine holds a key for cfg's project, minting one when
// it has none and a credential to mint with. It returns a line to print, or "" when there
// is nothing to say. It never fails the install: the hooks and the binding are what the
// repository needs, and held events wait up to the spool's MaxAge for a key.
func ensureSpoolKey(ctx context.Context, cfg *config.Config) string {
	if keystore.Get(cfg.ProjectID) != "" {
		return ""
	}
	const held = "Hook events from this machine are held until it has a key for the project — "
	if cfg.APIKey != "" {
		return held + "TERMA_API_KEY cannot mint one; unset it and run `terma install` again."
	}
	client, err := newClient(cfg)
	var key string
	if err == nil {
		key, _, err = client.CreateServerKey(ctx, cfg.ProjectID, "terma-cli@"+harness.Hostname(),
			"Created by terma install, for hook events")
	}
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return held + "sign in with `terma setup`, then run `terma install` again."
	case err != nil:
		return held + "minting one failed (" + err.Error() + "); run `terma install` again."
	}
	if err := keystore.Set(cfg.ProjectID, key); err != nil {
		return held + "storing it failed (" + err.Error() + ")."
	}
	return "Project key stored for this machine's hook events (" + keystore.Mask(key) + ")."
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
func connectHarnessesForRepo(cmd *cobra.Command, cfg *config.Config, agents []string, f installFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	var telemetryAgents []string
	for _, a := range telemetryAgentNames(agents) {
		if _, err := harness.Lookup(a); err == nil {
			telemetryAgents = append(telemetryAgents, a)
		}
	}
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
		rec.CLI = &cli
		rec.Desktop = &desktop
	}
	for _, a := range telemetryAgents {
		h, _ := harness.Lookup(a)
		key, _, _, _, err := resolveKey(ctx, cfg, h, connectFlags{})
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		if err := keystore.SetFor(a, cfg.ProjectID, key); err != nil {
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
			// .opencode/terma.json overlay, written by install below. Note it rather
			// than silently dropping a capture the flags imply the developer wanted.
			if !f.telemetry && (!f.excludePrompts || !f.excludeToolContent) {
				fmt.Fprintln(out, "  OpenCode     → per-repo plugin (prompt/tool-content capture off; enable with terma install)")
			} else {
				fmt.Fprintln(out, "  OpenCode     → per-repo plugin")
			}
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
	err = setupActivation(cmd, launchedFromShell, f)
	return err
}

// setupActivation installs the per-repo routing mechanism.
// The PATH shim is the default and is installed without asking; the shim scripts front
// the agent binaries and re-invoke terma. `--activation wrapper` opts into printed shell
// functions instead. The one thing terma cannot do silently is put the shim directory on
// PATH — that lives in the developer's shell rc — so it prints that line once, and only
// when the directory is not already there.
func setupActivation(cmd *cobra.Command, agents []string, f installFlags) error {
	out := cmd.OutOrStdout()
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
		fmt.Fprintf(out, "  Routing      → PATH shims for %s in %s\n", joinNames(adapterDisplayNames(agents)), binDir)
		if !allActive(agents) {
			putShimsOnPath(cmd, binDir, f)
		}
	case "wrapper":
		if _, err := shim.InstallShims(agents); err != nil {
			return err
		}
		fmt.Fprintln(out, "\nAdd these to your ~/.zshrc (or ~/.bashrc) so the agents route to this repo's project:")
		fmt.Fprint(out, indent(shim.WrapperSnippet(agents)))
	default:
		return fmt.Errorf("unknown --activation %q (want shim or wrapper)", mode)
	}
	return nil
}

// putShimsOnPath gets the shim directory onto PATH ahead of the real binaries — the one
// step of per-repo routing that lives in the developer's shell startup file. It asks
// before writing there (--yes answers; --no-path declines), writes one marked block at
// the end of the file, and says what is left to do, which is always "open a new terminal":
// the shell install was run from read that file before the block was in it.
//
// The block goes last on purpose. A PATH line only wins over the ones that run after it,
// and a real startup file prepends ~/.local/bin — where the real claude and codex live —
// more than once; pasted anywhere but the end, terma's line is silently overtaken.
func putShimsOnPath(cmd *cobra.Command, binDir string, f installFlags) {
	out := cmd.OutOrStdout()
	rc, ok := shim.ShellRC()
	manual := func(why string) {
		line := `export PATH="` + binDir + `:$PATH"`
		if ok {
			line = rc.PathLine(binDir)
		}
		fmt.Fprintf(out, "  %s as the LAST line that touches PATH in your shell's startup file — a later line that\n", why)
		fmt.Fprintln(out, "  prepends another directory puts the real binaries back in front:")
		fmt.Fprintf(out, "    %s\n", line)
	}
	if !ok || f.noPath {
		manual("Add this")
		return
	}
	state, err := rc.State()
	if err != nil {
		manual("Could not read " + tildePath(rc.Path) + " (" + err.Error() + "). Add this")
		return
	}
	question := "Add terma's shim directory to PATH in " + tildePath(rc.Path) + "?"
	switch state {
	case shim.RCLast:
		fmt.Fprintf(out, "  %s already puts the shims first — open a new terminal; this one started before that line did.\n", tildePath(rc.Path))
		return
	case shim.RCOvertaken:
		question = "A later line in " + tildePath(rc.Path) + " puts the real binaries back in front. Move terma's PATH line to the end?"
	}
	// Consent is --yes, or a yes typed at a terminal. Anything else — a script, an agent, a
	// declined prompt — leaves the file alone.
	consented := f.assumeYes
	if !consented && canPrompt() {
		fmt.Fprintln(out)
		consented = confirmYes(cmd, question)
	}
	if !consented {
		manual("Not written. Add this")
		return
	}
	if _, err := rc.Ensure(binDir); err != nil {
		manual("Could not write " + tildePath(rc.Path) + " (" + err.Error() + "). Add this")
		return
	}
	fmt.Fprintf(out, "  PATH         → %s (last line; `terma shim uninstall` removes it). Open a new terminal for it to take effect.\n", tildePath(rc.Path))
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
// it outright; otherwise it is the union of what a prior install already committed, the
// agents this install configures, and any adapter whose directory the repository already
// carries (a .cursor / .codex / .agents directory is a clear sign the repo is opened in
// that agent) — restricted to adapters that actually write a hooks file.
//
// A union (rather than replacing with the current selection) means selecting more agents
// grows the committed set, while a colleague re-running install with a narrower selection
// never removes hooks someone else committed — so the file grows on purpose and never
// churns down.
func installAdapters(root string, agents []string, override string, existing *termaproject.File) []string {
	if list := splitCommas(override); len(list) > 0 {
		return list
	}
	want := map[string]bool{}
	if existing != nil {
		for _, a := range existing.Install.Adapters {
			want[a] = true
		}
	}
	for _, a := range agents {
		if a == codexDesktopAgent {
			want[shim.AgentCodex] = true
		} else {
			want[a] = true
		}
	}
	var out []string
	for _, a := range adapter.All() {
		if a.HooksPath() == "" {
			continue
		}
		if want[a.Name()] || a.Default(root) {
			out = append(out, a.Name())
		}
	}
	return out
}

// committedAdapters is what the binding already records as wired — what install keeps
// when it writes no hooks this time, so the committed file never names an adapter whose
// hooks file was not written. Nil on a fresh install.
func committedAdapters(existing *termaproject.File) []string {
	if existing == nil {
		return nil
	}
	return existing.Install.Adapters
}

// hookPlan is everything install would write into the repository for hooks: the
// commit hooks through the detected manager, and each wired agent's own hooks file.
type hookPlan struct {
	det    hookmgr.Detection
	hooks  hookmgr.Plan
	agents []hookmgr.Plan
}

func planHooks(root string, det hookmgr.Detection, adapters []string) (hookPlan, error) {
	hooks, err := hookmgr.PlanInstall(root, det)
	if err != nil {
		return hookPlan{}, err
	}
	agents, err := planAdapters(root, adapters, true)
	if err != nil {
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
	fmt.Fprintf(out, "\nHooks — commit stamping via %s (%s):\n", p.det.Manager, p.det.Detail)
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
// A path is listed once, even when two changes touched it.
func printCommitList(out io.Writer, paths []string) {
	var unique []string
	for _, p := range paths {
		if !slices.Contains(unique, p) {
			unique = append(unique, p)
		}
	}
	fmt.Fprintln(out, "Commit these files and open a PR — merging it onboards the repository:")
	for _, p := range unique {
		fmt.Fprintf(out, "  %s\n", p)
	}
	fmt.Fprintf(out, "\n  git add %s\n", strings.Join(unique, " "))
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

// confirmYes prompts and treats a read error as "no".
func confirmYes(cmd *cobra.Command, question string) bool {
	ok, err := confirm(cmd, question)
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

// indent prefixes each non-empty line with two spaces, for a printed snippet.
func indent(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// binding is the project a repository is tied to.
type binding struct {
	ID, Name, OrganizationID string
}

// resolveBinding picks the project: an explicit reference (matched against the
// organization's projects, falling back to a picker when it does not match), else the
// existing binding, else a picker.
func resolveBinding(cmd *cobra.Command, cfg *config.Config, existing *termaproject.File, ref string) (binding, error) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		client, err := newClient(cfg)
		if err != nil {
			if errors.Is(err, auth.ErrNotLoggedIn) {
				return binding{ID: ref}, nil
			}
			return binding{}, err
		}
		projects, err := fetchProjects(cmd.Context(), client)
		if err != nil {
			return binding{}, err
		}
		if p, err := matchProject(projects, ref); err == nil {
			return binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID}, nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "No project matches %q in this organization — pick one:\n", ref)
		p, err := pickProject(cmd, projects)
		if err != nil {
			return binding{}, err
		}
		return binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID}, nil
	}
	if existing != nil {
		return binding{ID: existing.Project.ID, Name: existing.Project.Name, OrganizationID: existing.Project.OrganizationID}, nil
	}
	p, err := chooseProject(cmd, cfg)
	if err != nil {
		return binding{}, err
	}
	return binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID}, nil
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
			root, gitDir, err := repoHere(ctx, "terma uninstall runs inside a git repository")
			if err != nil {
				return err
			}
			existing, _ := termaproject.Load(root)
			det := hookmgr.Detect(root)
			if existing != nil && existing.Install.HookManager != "" {
				det.Manager = hookmgr.Manager(existing.Install.HookManager)
				if det.Manager == hookmgr.GitShim {
					det.ConfigPath = hookmgr.ShimDir
				}
			}
			hooks, err := hookmgr.PlanUninstall(root, det)
			if err != nil {
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
			if hp := gitx.ConfigGet(ctx, root, "core.hooksPath"); hp == hookmgr.ShimDir {
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
			for _, h := range repoPolicyHarnesses(installedAdapters(existing)) {
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
			if err := unwireRepo(ctx, root, gitDir); err != nil {
				return err
			}
			if err := termaproject.Remove(root); err != nil {
				return err
			}
			if err := session.Open(gitDir).Remove(); err != nil {
				return err
			}
			fmt.Fprintln(out, "Uninstalled. Commit the removals if the install was committed.")
			fmt.Fprintln(out, "Per-repo routing (a PATH shim and routing records) stays on your machine — run `terma shim uninstall` when you no longer route any repo.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
