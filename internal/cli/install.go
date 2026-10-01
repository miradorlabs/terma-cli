package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type installFlags struct {
	projectRef   string
	harnesses    string
	adapters     string
	noHooks      bool
	noDoctor     bool
	noStatusLine bool
	// relayService and prompts are "on", "off", or "" to keep the last choice.
	relayService       string
	identity           string
	signals            string
	prompts            string
	excludePrompts     bool
	excludeToolContent bool
	updatePolicy       bool
	noBrowser          bool
	dryRun             bool
	assumeYes          bool
	force              bool
	verbose            bool
}

func (app *App) newInstallCommand() *cobra.Command {
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
  2. Points each of your agents at that project, through the local relay: their own
     exporters (or terma's plugin, for an agent without a usable one) send to a relay
     on this machine, and the relay forwards only the sessions this repository's hooks
     claim, with the project's key. Nothing else leaves the machine. Keys stay in your home
     directory, namespaced by project — never in the repository. Prompt text and model
     responses are sent (your last choice for the project, on for a first install);
     --prompts off stops them. A desktop app also reports through repository hooks.
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
			return app.runInstall(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.projectRef, "project", "", "Terma project (name or id) to bind the repository to")
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to configure ("+strings.Join(app.availableAgentNames(), ", ")+"); default: what `terma setup` recorded, else a picker")
	cmd.Flags().StringVar(&f.adapters, "adapters", "", "comma-separated agents whose committed hooks to wire (default: the configured agents that have one)")
	cmd.Flags().BoolVar(&f.noHooks, "no-hooks", false, "do not install commit hooks or agent hooks")
	cmd.Flags().BoolVar(&f.noDoctor, "no-doctor", false, "do not run `terma doctor` to verify the chain after installing")
	cmd.Flags().StringVar(&f.relayService, "relay-service", "", "run the local relay as a background service: on or off (default: on, or your last choice)")
	cmd.Flags().BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap "+app.statusLineOwner()+"'s status line (which captures the plan's rate-limit windows)")
	cmd.Flags().StringVar(&f.identity, "identity", "", "identity stamped on the sessions of agents that take one (default: git user.email; \"none\" to omit)")
	cmd.Flags().StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
	cmd.Flags().StringVar(&f.prompts, "prompts", "", "send prompt text and model responses: on or off (default: your last choice for this project, on for a first install)")
	cmd.Flags().BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	// --exclude-prompts is --prompts off, kept for the scripts that pass it.
	_ = cmd.Flags().MarkHidden("exclude-prompts")
	cmd.Flags().BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "show what would change without writing anything")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "show setup steps and what each step wrote")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace conflicting harness settings instead of refusing")
	return cmd
}

func (app *App) runInstall(cmd *cobra.Command, f installFlags) error {
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

	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	// A dry run is nothing but its plan, so it always says everything.
	ui := newInstallUI(out, f.verbose || f.dryRun)
	fmt.Fprintf(out, "%s in %s\n\n", ui.p.Bold("Installing terma"), output.TildePath(root))
	for _, path := range append([]string{termaproject.FileName}, app.agents.HooksPaths()...) {
		if err := termaproject.CheckPath(root, path); err != nil {
			return err
		}
	}
	// A linked worktree keeps its main checkout's project; the binding it writes is its own.
	existing, _, err := termaproject.Resolve(root, gitDir)
	if err != nil && !errors.Is(err, termaproject.ErrNotFound) {
		return err
	}

	// Agents are resolved first because they decide whether sign-in is needed.
	agents, err := app.resolveInstallHarnesses(cmd, cfg, f)
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	if err != nil {
		return err
	}
	if err := install.CheckSignalNeeds(app.agents, agents, f.signals); err != nil {
		return err
	}
	// Flags that disagree are refused before anything signs in or prints.
	prompts, toolContent, err := contentChoices(cmd, f)
	if err != nil {
		return err
	}
	switch f.relayService {
	case "", "on", "off":
	default:
		return fmt.Errorf("--relay-service %q: want on or off", f.relayService)
	}

	// Every real install reads the team's policy, even hooks-only; only an offline policy
	// fixture skips that login. A --dry-run never signs in.
	needsAuth := cfg.APIKey == "" && (os.Getenv("TERMA_POLICY_STUB") == "" || app.installNeedsAuth(agents, f.projectRef, existing, !f.noHooks))
	if needsAuth && !f.dryRun {
		if cfg, err = app.signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser}); err != nil {
			return err
		}
	}

	b, err := app.resolveBinding(cmd, cfg, existing, f.projectRef, needsAuth, !f.assumeYes && !f.dryRun && canPrompt())
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	if err != nil {
		// A signed-out dry run plans against an unresolved project: its files do not
		// depend on the project id.
		if !f.dryRun || !errors.Is(err, auth.ErrNotLoggedIn) {
			return err
		}
		b = install.Binding{}
	}
	cfg.ProjectID, cfg.ProjectName, cfg.OrganizationID = b.ID, b.Name, b.OrganizationID
	var admitting *config.Policy
	if !f.dryRun {
		pol, err := app.fetchPolicy(ctx, cfg)
		if err != nil {
			return err
		}
		if err := install.Admit(pol, existing, b); err != nil {
			return err
		}
		if err := saveCollectionPolicy(cfg, &pol); err != nil {
			return err
		}
		cfg.Policy, admitting = pol, &pol
	}
	// Built once, so a dry run prints exactly the plan an install applies.
	plan, err := install.Build(app.agents, install.Input{Root: root, GitDir: gitDir, Existing: existing, Selected: agents,
		Adapters: splitCommas(f.adapters), NoHooks: f.noHooks, Binding: b, Prompts: prompts, ToolContent: toolContent, Policy: admitting})
	if err != nil {
		return err
	}
	f.excludePrompts, f.excludeToolContent = !plan.Prompts, !plan.ToolContent

	if gitDir == "" {
		ui.Warn("Git hooks", "skipped — not a Git repository, so commits are not stamped")
	}
	env := ""
	if cfg.Environment != config.EnvProd {
		env = " (" + cfg.Environment + ")"
	}
	if b.ID == "" && b.Name == "" {
		ui.Warn("Project", "unresolved — a real install signs in and selects one"+env)
	} else {
		ui.summary("Project", cmp.Or(b.Name, b.ID)+env)
	}

	// Nothing asks, so the line names the command that changes it.
	if len(app.agents.RelayTargets(agents)) > 0 {
		if plan.Prompts {
			ui.summary("Prompts", "prompt text and model responses are sent — `terma install --prompts off` stops them")
		} else {
			ui.summary("Prompts", "prompt text and model responses are not sent — `terma install --prompts on` sends them")
		}
	}

	if f.dryRun {
		return plan.PrintDryRun(out, needsAuth)
	}
	steps := install.Steps{
		Confirm: func(question string, explain []string) (bool, error) {
			yes, err := confirmExplained(cmd, question, explain, true)
			if errors.Is(err, errCancelled) {
				return false, err
			}
			return err == nil && yes, nil
		},
		// The per-developer half: home-directory state, no committed file.
		Connect: func(context.Context) error { return app.connectHarnessesForRepo(cmd, ui, cfg, agents, f, plan.Record) },
		SpoolKey: func(ctx context.Context) (string, string) {
			sp := spinner.New(cmd.ErrOrStderr())
			sp.Start("Preparing hook event delivery…")
			defer sp.Stop()
			k := app.ensureSpoolKey(ctx, cfg)
			return k.state, k.fix
		},
		RepoPolicy: func(ctx context.Context, hs []harness.Harness) ([]string, error) {
			return writeRepoPolicy(ctx, ui, root, cfg, hs, f)
		},
	}
	// The status line is a global setting, wrapped only for a developer who chose its agent.
	if a, ok := doctor.StatusLineAgent(app.agents); ok && !f.noStatusLine && slices.Contains(agents, a.Name()) {
		steps.StatusLine = func() (string, bool) { return app.installStatusLine(cmd.ErrOrStderr()) }
	}
	if err := install.Apply(ctx, plan, install.Options{AssumeYes: f.assumeYes, Version: app.version, Now: time.Now()}, steps, ui); err != nil {
		return err
	}
	// A newer release's first install refreshes the machine before doctor checks it, and
	// records it so the refresh after the command has nothing left to do.
	if dir, err := config.Dir(); err == nil && selfupdate.NeedsRefresh(dir, app.version) {
		changed, err := app.refreshMachine()
		for _, p := range changed {
			fmt.Fprintf(ui.detail, "  updated %s\n", p)
		}
		if err != nil {
			ui.Warn("Refreshed", "some files an earlier terma installed could not be updated ("+err.Error()+")")
			ui.Then("Run `terma update --refresh` to retry.")
		} else {
			if len(changed) > 0 {
				ui.OK("Refreshed", fmt.Sprintf("%d file(s) an earlier terma installed", len(changed)))
			}
			_ = selfupdate.SaveRefreshed(dir, app.version)
		}
	}

	// Skipped without a terminal: doctor makes a scratch commit and a network round-trip.
	if f.noDoctor || !canPrompt() {
		ui.Then("Run `terma doctor` to verify the chain end to end.")
	} else {
		ui.verify(cmd, app.runDoctor)
	}
	ui.finish()
	return nil
}

// contentChoices are the explicit --prompts / --exclude-prompts and
// --exclude-tool-content choices, nil where the developer made none.
func contentChoices(cmd *cobra.Command, f installFlags) (prompts, toolContent *bool, err error) {
	excluded := cmd.Flags().Changed("exclude-prompts") && f.excludePrompts
	switch strings.ToLower(strings.TrimSpace(f.prompts)) {
	case "on":
		if excluded {
			return nil, nil, errors.New("--prompts on and --exclude-prompts disagree; pass one")
		}
		prompts = new(true)
	case "off":
		prompts = new(false)
	case "":
		if excluded {
			prompts = new(false)
		}
	default:
		return nil, nil, fmt.Errorf("--prompts %q: want on or off", f.prompts)
	}
	if cmd.Flags().Changed("exclude-tool-content") {
		toolContent = new(!f.excludeToolContent)
	}
	return prompts, toolContent, nil
}

// installNeedsAuth includes minting the spool key, which hooks-only agents get from
// nowhere else.
func (app *App) installNeedsAuth(agents []string, projectRef string, existing *termaproject.File, wantsHooks bool) bool {
	for _, a := range app.telemetryAgentNames(agents) {
		if _, err := app.agents.Harness(a); err == nil {
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

func (app *App) telemetryAgentNames(agents []string) []string {
	var names []string
	for _, name := range agents {
		if _, a, ok := app.agents.Surface(name); ok {
			name = a.Name()
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func projectRefNeedsLookup(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	return !termaproject.ValidID(ref)
}

// spoolKey's fix, when set, is what the developer must do before events are delivered.
type spoolKey struct{ state, fix string }

// ensureSpoolKey never fails the install: held events wait up to the spool's MaxAge for a key.
func (app *App) ensureSpoolKey(ctx context.Context, cfg *config.Config) spoolKey {
	if keystore.Get(cfg.ProjectID) != "" {
		return spoolKey{state: "delivered with this project's key"}
	}
	const held = "held until this machine has a key for the project"
	if cfg.APIKey != "" {
		return spoolKey{held, "TERMA_API_KEY cannot mint a key for hook events: unset it and run `terma install` again."}
	}
	client, err := app.newClient(cfg)
	var key string
	if err == nil {
		key, _, err = client.CreateServerKey(ctx, cfg.ProjectID, "terma-cli@"+hostname(),
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

// resolveInstallHarnesses never reads the committed binding: agents are a per-developer choice.
func (app *App) resolveInstallHarnesses(cmd *cobra.Command, cfg *config.Config, f installFlags) ([]string, error) {
	if h := strings.TrimSpace(f.harnesses); h != "" {
		if strings.EqualFold(h, "none") {
			return nil, nil
		}
		return app.parseAgentList(f.harnesses)
	}
	if len(cfg.Harnesses) > 0 {
		chosen := map[string]bool{}
		for _, name := range cfg.Harnesses {
			chosen[name] = true
		}
		if names := app.selectedInRegistryOrder(chosen); len(names) > 0 {
			return names, nil
		}
	}
	names, err := app.chooseHarnesses(cmd, cfg, setupFlags{assumeYes: f.assumeYes})
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

// connectHarnessesForRepo writes no committed file: keys, the routing record and the
// relay are home-directory state (docs/RELAY.md).
func (app *App) connectHarnessesForRepo(cmd *cobra.Command, ui *installUI, cfg *config.Config, agents []string, f installFlags, prev *routing.Record) error {
	ctx := cmd.Context()
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	targets := app.agents.RelayTargets(agents)
	if len(targets) == 0 {
		return nil
	}
	rec := routing.Record{
		ProjectID:          cfg.ProjectID,
		Endpoint:           cfg.OTLPURL,
		Signals:            signalStrings(signals),
		IncludePrompts:     !f.excludePrompts,
		IncludeToolContent: !f.excludeToolContent,
		Harnesses:          targets,
		Surfaces:           install.RoutedSurfaces(app.agents, agents, targets),
	}
	if !cmd.Flags().Changed("signals") && prev != nil {
		rec.Signals = prev.Signals
	}
	sp := spinner.New(cmd.ErrOrStderr())
	defer sp.Stop()
	for _, a := range targets {
		h, err := app.agents.Harness(a)
		if err != nil {
			continue // an exporter terma writes: it sends with the project's spool key
		}
		// Either key on file serves the relay, so none is minted.
		if keystore.GetFor(a, cfg.ProjectID) != "" || keystore.Get(cfg.ProjectID) != "" {
			continue
		}
		sp.Start("Preparing " + h.DisplayName() + "'s key for this project…")
		key, _, _, _, err := app.resolveKey(ctx, cfg, h, connectFlags{})
		sp.Stop()
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		if err := keystore.SetFor(a, cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
			return err
		}
	}
	if err := routing.SaveRecord(rec); err != nil {
		return err
	}
	// The machine half too, so an install without a setup is complete.
	err = app.connectMachineRelay(ctx, agents, f.relayService, relayReport{ok: ui.OK, warn: ui.Warn, then: ui.Then, detail: ui.detail})
	if err != nil {
		return err
	}
	for _, s := range install.SelectedSurfaces(app.agents, agents) {
		if s.Reports != "" {
			ui.OK(s.DisplayName, s.Reports)
		}
	}
	return nil
}

// printCommitList exists because the hooks do nothing for a colleague until the files are merged.
func printCommitList(out io.Writer, lead string, paths []string) {
	fmt.Fprintln(out, commitList(style.For(out), lead, paths))
}

func signalStrings(signals []harness.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, string(s))
	}
	return out
}

func boundTo(p *project, cfg *config.Config) install.Binding {
	return install.Binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: nonProd(cfg.Environment)}
}

// resolveBinding with verify checks the binding against the credential's projects: one
// from another environment or organization would be refused at the first key mint.
func (app *App) resolveBinding(cmd *cobra.Command, cfg *config.Config, existing *termaproject.File, ref string, verify, ask bool) (install.Binding, error) {
	sp := spinner.New(cmd.ErrOrStderr())
	sp.Start("Loading projects…")
	defer sp.Stop()
	ref = strings.TrimSpace(ref)
	if serverkey.Is(cfg.APIKey) {
		return app.serverKeyBinding(cmd.Context(), cfg, existing, ref)
	}
	current := ""
	if existing != nil {
		current = existing.Project.ID
	}
	if ref != "" {
		client, err := app.newClient(cfg)
		if err != nil {
			if errors.Is(err, auth.ErrNotLoggedIn) {
				return install.Binding{ID: ref, Environment: nonProd(cfg.Environment)}, nil
			}
			return install.Binding{}, err
		}
		projects, err := fetchProjects(cmd.Context(), client)
		sp.Stop()
		if err != nil {
			return install.Binding{}, err
		}
		if p, err := matchProject(projects, ref); err == nil {
			return boundTo(p, cfg), nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "No project matches %q in this organization — pick one:\n", ref)
		p, err := pickProject(cmd, projects, current)
		if err != nil {
			return install.Binding{}, err
		}
		return boundTo(p, cfg), nil
	}
	if existing != nil && !verify {
		return install.Kept(existing), nil
	}

	client, err := app.newClient(cfg)
	if err != nil {
		if existing != nil && errors.Is(err, auth.ErrNotLoggedIn) {
			return install.Kept(existing), nil
		}
		return install.Binding{}, err
	}
	projects, err := availableProjects(cmd.Context(), client)
	sp.Stop()
	if errors.Is(err, errNoProjects) {
		return install.Binding{}, fmt.Errorf("%w, then run `terma install` again", err)
	}
	if err != nil {
		return install.Binding{}, err
	}
	bound := existing != nil && slices.ContainsFunc(projects, func(p project) bool { return p.ID == current })

	switch {
	case existing != nil && !bound && !ask:
		return install.Binding{}, fmt.Errorf("%s — run `terma install --project <name or id>` with one of yours (`terma project list` lists them)", unreachableBinding(existing, cfg))
	case existing != nil && !bound:
		reason := unreachableBinding(existing, cfg)
		fmt.Fprintf(cmd.ErrOrStderr(), "%s%s.\n", strings.ToUpper(reason[:1]), reason[1:])
		current = ""
	case bound && !ask:
		return install.Kept(existing), nil
	}

	p, err := soleOrPick(cmd, projects, current)
	if err != nil {
		return install.Binding{}, err
	}
	if bound && p.ID == current {
		return install.Kept(existing), nil
	}
	return boundTo(p, cfg), nil
}

func unreachableBinding(existing *termaproject.File, cfg *config.Config) string {
	p := existing.Project
	org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
	if org == "" {
		org = "your organization"
	}
	msg := fmt.Sprintf("this repository is bound to %s, which is not a project in %s", cmp.Or(p.Name, p.ID), org)
	switch {
	case !config.SameAccounts(p.Environment, cfg.Environment):
		return fmt.Sprintf("%s: it was bound in %s, and terma is using %s", msg, environmentLabel(p.Environment), environmentLabel(cfg.Environment))
	case p.OrganizationID != "" && p.OrganizationID != cfg.OrganizationID:
		return fmt.Sprintf("%s: it belongs to organization %s (`terma org use %s` switches to it, if you are a member)", msg, p.OrganizationID, p.OrganizationID)
	}
	return msg
}

func environmentLabel(env string) string {
	if env == "" || env == config.EnvProd {
		return "production"
	}
	return "the " + env + " environment"
}

// serverKeyBinding asks the gateway's /v1/identity, since the account service lists
// projects only for a signed-in user; another project is refused, as the key cannot
// deliver its events.
func (app *App) serverKeyBinding(ctx context.Context, cfg *config.Config, existing *termaproject.File, ref string) (install.Binding, error) {
	client, err := app.newClient(cfg)
	if err != nil {
		return install.Binding{}, err
	}
	var identity struct {
		ProjectID      string `json:"project_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := client.Get(ctx, "/v1/identity", nil, &identity); err != nil {
		return install.Binding{}, fmt.Errorf("look up the project TERMA_API_KEY belongs to: %w", err)
	}
	if identity.ProjectID == "" {
		return install.Binding{}, errors.New("TERMA_API_KEY names no project")
	}
	if ref != "" && ref != identity.ProjectID {
		return install.Binding{}, fmt.Errorf("TERMA_API_KEY belongs to project %s, not %q — a server key binds only its own project, named by id", identity.ProjectID, ref)
	}
	b := install.Binding{ID: identity.ProjectID, OrganizationID: identity.OrganizationID, Environment: nonProd(cfg.Environment)}
	if existing != nil {
		if existing.Project.ID != identity.ProjectID {
			return install.Binding{}, fmt.Errorf("this repository is bound to project %s, and TERMA_API_KEY belongs to %s", existing.Project.ID, identity.ProjectID)
		}
		b.Name = existing.Project.Name
	}
	return b, nil
}

func nonProd(env string) string {
	if env == config.EnvProd {
		return ""
	}
	return env
}

func splitCommas(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (app *App) newUninstallCommand() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything terma install wrote to this repository",
		Long: `Symmetric with install: removes terma's hook wiring (only terma's lines from
shared hook files), each agent's committed hooks, .terma/settings.json, the per-clone
git configuration, and the local session state. Removing the binding un-routes this
checkout; the home-dir routing state (keys, routing records) is kept, since it is shared
with any other worktree or clone bound to the same project — remove it machine-wide
with 'terma nate'.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			root, gitDir, err := workspaceHere(ctx)
			if err != nil {
				return err
			}
			for _, path := range append([]string{termaproject.FileName}, app.agents.HooksPaths()...) {
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
			plans, err := install.PlanAdapters(app.agents, root, app.agents.Names(), false)
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
			// Every scoped harness, not only the ones an install chose.
			for _, h := range app.agents.Harnesses() {
				scoped, ok := h.(harness.Scoped)
				if !ok {
					continue
				}
				if _, err := scoped.Local(root).Disconnect(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not remove %s's repository policy: %v\n", h.DisplayName(), err)
				}
			}
			// Routing records and keys are per project, shared with other checkouts, so they
			// stay; removing the binding is what stops routing here.
			if gitDir != "" {
				if err := install.Unwire(ctx, root, gitDir); err != nil {
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
				// A workspace that gained Git keeps private session storage, but its hook
				// restoration journal lives in the worktree metadata.
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
			fmt.Fprintln(out, "This machine's routing records and keys stay — run `terma nate` when you no longer route any repository.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
