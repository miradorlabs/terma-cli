package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/prompt"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type setupFlags struct {
	harnesses string
	noBrowser bool
	assumeYes bool
	// relayService is --relay-service: "on", "off", or "" (keep the recorded choice; on
	// where a service can run).
	relayService string
	// managedConfig is --managed-config: write global mode's hooks as managed
	// configuration into this directory, for an organization to deploy, and do nothing
	// else. managedTerma is the path those hooks call terma by.
	managedConfig string
	managedTerma  string
}

// agentChoice is an onboarding surface. Codex CLI and Codex desktop share one
// repository adapter, but developers choose independently how they launch it.
type agentChoice struct {
	name      string
	display   string
	installed func(context.Context) bool
}

func (a agentChoice) Name() string        { return a.name }
func (a agentChoice) DisplayName() string { return a.display }
func (a agentChoice) Installed(ctx context.Context) bool {
	return a.installed(ctx)
}

func (app *App) newSetupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Sign in and choose your coding agents (once per developer)",
		Long: `Gets this machine ready to use terma, once per developer:

  1. Signs you in (a browser handoff; --no-browser prints the URL instead).
  2. Records which coding agents you work with (an agent's CLI and desktop app
     separately).
  3. Fetches your organization's collection policy.
  4. Points those agents' telemetry at terma's local relay, and runs the relay in
     the background (--relay-service off: started on demand instead). Only sessions
     allowed by the selected team's collection policy leave this machine.

A repository your organization connected in Terma needs nothing more: its committed
hooks claim its sessions. ` + "`terma install`" + ` connects a repository from here instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runSetup(cmd, f) },
	}
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to record ("+strings.Join(app.availableAgentNames(), ", ")+"); default: a picker")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "skip the browser prompt and picker; record every available installed agent")
	cmd.Flags().StringVar(&f.relayService, "relay-service", "", "run the local relay as a background service: on or off (default: on, or your last choice)")
	cmd.Flags().StringVar(&f.managedConfig, "managed-config", "", "write global mode's hooks as managed configuration into this directory, for your organization to deploy, and exit")
	cmd.Flags().StringVar(&f.managedTerma, "managed-terma", "$HOME/.local/bin/terma", "with --managed-config: where terma is installed on the machines")
	return cmd
}

func (app *App) runSetup(cmd *cobra.Command, f setupFlags) error {
	out := cmd.OutOrStdout()
	if f.managedConfig != "" {
		files, err := app.writeManagedConfig(f.managedConfig, f.managedTerma)
		if err != nil {
			return err
		}
		for _, p := range files {
			fmt.Fprintln(out, "Wrote "+output.TildePath(p))
		}
		fmt.Fprintln(out, "Deploy them as the README there says; each developer still runs `terma setup` once.")
		return nil
	}
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	// A welcome for a person watching: the logo, and beside it who and where terma is
	// pointed right now. Suppressed off a terminal (a pipe, an agent, NO_COLOR), like the
	// spinner.
	if p := style.For(out); p.Enabled() {
		fmt.Fprintln(out, style.Header(p, app.setupHeaderInfo(cfg, p)))
	}
	if cfg.APIKey != "" {
		return errors.New("TERMA_API_KEY is set — setup signs in as a person; unset it first")
	}
	switch f.relayService {
	case "", "on", "off":
	default:
		return fmt.Errorf("--relay-service %q: want on or off", f.relayService)
	}

	// 1. Sign in — reusing the session this machine already has, verified.
	if cfg, err = app.signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser, pauseBeforeBrowser: !f.assumeYes}); err != nil {
		return err
	}

	// 2. Which agents this developer uses. A machine-level preference, not a connection.
	fmt.Fprintln(out)
	names, err := app.chooseHarnesses(cmd, cfg, f)
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was recorded.")
		return nil
	}
	if err != nil {
		return err
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { p.Harnesses = names }); err != nil {
		return err
	}

	if len(names) == 0 {
		fmt.Fprintln(out, "\nNo agents recorded. `terma install` will ask you to pick some in each repository.")
	} else {
		fmt.Fprintf(out, "\nAgents recorded: %s.\n", joinNames(app.adapterDisplayNames(names)))
	}

	// 3. What the organization collects. Kept on the profile: hooks and the relay read
	// it there and never ask the network.
	if err := app.selectPolicyTeam(cmd, cfg); err != nil {
		return err
	}
	pol, err := app.fetchPolicy(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	previous := cfg.Policy
	cfg.Policy = pol
	if err := saveCollectionPolicy(cfg, &pol); err != nil {
		return err
	}
	// A relay's login and environment are fixed at startup. Scope changes require
	// restarting it; capture-only changes are picked up from the local caches.
	if previous.OrganizationID != pol.OrganizationID || previous.AuthURL != pol.AuthURL || previous.TeamID != pol.TeamID {
		if dir, err := daemon.Dir(); err == nil {
			daemon.Stop(dir)
		}
	}
	fmt.Fprintln(out, "Collection policy: "+policySummary(pol)+".")

	// 4. The relay: the machine half of every repository's telemetry.
	var steps []string
	err = app.connectMachineRelay(cmd.Context(), names, f.relayService, relayReport{
		ok:     func(label, what string) { fmt.Fprintf(out, "  %s: %s\n", label, what) },
		warn:   func(label, what string) { fmt.Fprintf(out, "  %s (needs you): %s\n", label, what) },
		then:   func(step string) { steps = append(steps, step) },
		detail: io.Discard,
	})
	if err != nil {
		return err
	}

	// 5. Global mode: hooks for every session and every commit on the machine, not only
	// repositories that opted in. Leaving global mode takes them away again.
	if err := app.applyGlobalMode(cmd.Context(), names, pol.Global(), func(what string) { fmt.Fprintln(out, "  "+what) },
		func(step string) { steps = append(steps, step) }); err != nil {
		return err
	}
	// 6. The check-in: the relay reports this machine to the organization now.
	if len(app.agents.RelayTargets(names)) > 0 {
		if ok, what := daemon.CheckIn(cmd.Context()); ok {
			fmt.Fprintln(out, "  Check-in: "+what)
		} else {
			fmt.Fprintln(out, "  Check-in (needs you): "+what)
		}
	}
	for i, step := range steps {
		fmt.Fprintf(out, "%d. %s\n", i+1, step)
	}

	if !pol.Global() {
		for _, n := range names {
			if s, _, ok := app.agents.Surface(n); ok {
				for _, step := range s.SetupSteps {
					fmt.Fprintln(out, step)
				}
			}
		}
	}
	if pol.Global() {
		fmt.Fprintf(out, "\n%s Every session and commit on this machine reports to your organization.\n", style.For(out).Bold("Done!"))
		return nil
	}
	fmt.Fprintf(out, "\n%s Repositories connected in Terma report on their own; run `terma install` to connect one from here.\n",
		style.For(out).Bold("Done!"))
	return nil
}

// applyGlobalMode puts global mode's machine-wide hooks in place (global), or takes
// away any an earlier global setup left (not global): the agents' user-level hooks and
// git's global hooks path. said reports what changed; then, what the developer must do.
func (app *App) applyGlobalMode(ctx context.Context, agents []string, global bool, said, then func(string)) error {
	files, err := app.applyUserHooks(agents, global)
	if err != nil {
		return err
	}
	for _, f := range files {
		said("Machine-wide hooks updated: " + output.TildePath(f))
	}
	if global && len(files) > 0 {
		for _, step := range app.userHooksTrustSteps(agents) {
			then(step)
		}
	}
	changed, err := app.applyGlobalGitHooks(ctx, global)
	if err != nil {
		return fmt.Errorf("git's global hooks: %w", err)
	}
	if changed && global {
		said("Git: every repository's commits are stamped (git config --global core.hooksPath); each repository's own hooks still run")
	} else if changed {
		said("Git: global hooks path restored")
	}
	return nil
}

// fetchPolicy asks the organization the developer signed in to for its collection
// policy.
func (app *App) fetchPolicy(ctx context.Context, cfg *config.Config) (config.Policy, error) {
	var client *api.Client
	var err error
	// An explicit offline fixture needs no credential. Production always uses the
	// normal client, which loads and refreshes the developer's login token.
	if os.Getenv("TERMA_POLICY_STUB") != "" {
		client = api.NewAnonymous(cfg.AuthURL, app.version)
	} else {
		cred, err := auth.LoadCredential(cfg.ProfileName)
		if err != nil {
			return config.Policy{}, err
		}
		if cfg.OrganizationID == "" {
			cfg.OrganizationID = cred.OrganizationID
		}
		if cfg.OrganizationID != cred.OrganizationID {
			return config.Policy{}, errors.New("collection policy login belongs to another organization — run `terma setup`")
		}
		// A server key can identify the binding and deliver its telemetry. Policy
		// always uses the developer login, even while TERMA_API_KEY is set.
		policyConfig := *cfg
		policyConfig.APIKey = ""
		client, err = api.New(&policyConfig, api.Options{Version: app.version, ProjectID: cfg.ProjectID, Credential: cred})
		if err != nil {
			return config.Policy{}, err
		}
	}
	pol, err := client.CollectionPolicy(ctx)
	if err != nil {
		return config.Policy{}, fmt.Errorf("fetch the organization's collection policy: %w", err)
	}
	pol.OrganizationID, pol.AuthURL = cfg.OrganizationID, cfg.AuthURL
	pol.TeamID = cmp.Or(cfg.ProjectID, pol.DefaultProjectID)
	if pol.Global() && os.Getenv("TERMA_POLICY_STUB") == "" {
		pol.DefaultProjectID = cfg.ProjectID
	}

	return pol, nil
}

// selectPolicyTeam names which team's policy the developer is setting up. It
// reads membership with the login token; it never creates a telemetry key.
func (app *App) selectPolicyTeam(cmd *cobra.Command, cfg *config.Config) error {
	if os.Getenv("TERMA_POLICY_STUB") != "" {
		return nil
	}
	if cfg.ProjectID == "" && cfg.Policy.TeamID != "" {
		cfg.ProjectID = cfg.Policy.TeamID
		return nil
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return err
	}
	projects, err := availableProjects(cmd.Context(), client)
	if err != nil {
		return err
	}
	var team *project
	if cfg.ProjectID != "" {
		team, err = matchProject(projects, cfg.ProjectID)
	} else {
		team, err = soleOrPick(cmd, projects, "")
	}
	if err != nil {
		return err
	}
	cfg.ProjectID = team.ID
	return nil
}

// policySummary says in a few words what the organization collects.
func policySummary(p config.Policy) string {
	scope := "sessions in connected repositories"
	if p.Global() {
		scope = "every session on this machine"
	}
	switch {
	case p.IncludePrompts && p.IncludeToolContent:
		return scope + ", with prompts and tool content"
	case p.IncludePrompts:
		return scope + ", with prompts, without tool content"
	case p.IncludeToolContent:
		return scope + ", with tool content, without prompts"
	}
	return scope + ", without prompts or tool content"
}

// chooseHarnesses resolves the machine-level agent list: the --harness flag if given,
// else a checkbox picker with coming-soon agents disabled (preselecting available
// agents already recorded or detected), else — with --yes or no terminal — the
// available recorded or detected agents.
func (app *App) chooseHarnesses(cmd *cobra.Command, cfg *config.Config, f setupFlags) ([]string, error) {
	if strings.TrimSpace(f.harnesses) != "" {
		return app.parseAgentList(f.harnesses)
	}
	ctx := cmd.Context()
	preselect := map[string]bool{}
	for _, n := range cfg.Harnesses {
		preselect[n] = true
	}
	for _, n := range app.detectedAgents(ctx) {
		preselect[n] = true
	}
	if f.assumeYes || !canPrompt() {
		return app.selectedInRegistryOrder(preselect), nil
	}

	form := app.harnessSelectionForm(ctx, preselect)
	result, err := prompt.Run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for i, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && result[i].Selected {
			names = append(names, a.Name())
		}
	}
	return names, nil
}

func (app *App) availableAgentNames() []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) {
			names = append(names, a.Name())
		}
	}
	return names
}

// harnessSelectionAgents puts available agents first, preserving registry order
// within each group. Both the form and its result mapping must use this order.
func (app *App) harnessSelectionAgents() []agentChoice {
	var choices []agentChoice
	for _, available := range []bool{true, false} {
		for _, a := range app.agents.All() {
			if app.agents.IsSupported(a.Name()) == available {
				for _, surface := range agents.Surfaces(a) {
					choices = append(choices, agentChoice{name: surface.Name, display: surface.DisplayName, installed: surface.Installed})
				}
			}
		}
	}
	return choices
}

func (app *App) harnessSelectionForm(ctx context.Context, preselect map[string]bool) *prompt.Form {
	form := &prompt.Form{Title: "Which coding agents do you use? (space toggles, enter confirms)"}
	for _, a := range app.harnessSelectionAgents() {
		item := prompt.Item{Label: a.DisplayName(), Kind: prompt.Check}
		if app.agents.IsSupported(a.Name()) {
			item.Detail = app.agentDetail(ctx, a.Name())
			item.Selected = preselect[a.Name()]
		} else {
			item.Disabled = true
			item.Reason = "Coming Soon"
		}
		form.Items = append(form.Items, item)
	}
	for _, name := range app.agents.UpcomingNames() {
		form.Items = append(form.Items, prompt.Item{Label: name, Kind: prompt.Check, Disabled: true, Reason: "Coming Soon"})
	}
	return form
}

// parseAgentList validates a comma-separated list of adapter names.
func (app *App) parseAgentList(raw string) ([]string, error) {
	seen := map[string]bool{}
	for _, n := range splitCommas(raw) {
		if _, _, ok := app.agents.Surface(n); !ok {
			return nil, fmt.Errorf("unknown agent %q (want %s)", n, joinNames(app.availableAgentNames()))
		}
		if !app.agents.IsSupported(n) {
			return nil, fmt.Errorf("agent %q: Coming Soon; available agents: %s", n, joinNames(app.availableAgentNames()))
		}
		seen[n] = true
	}
	return app.selectedInRegistryOrder(seen), nil
}

// selectedInRegistryOrder returns available chosen adapter names in registry order.
func (app *App) selectedInRegistryOrder(chosen map[string]bool) []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && chosen[a.Name()] {
			names = append(names, a.Name())
		}
	}
	return names
}

// detectedAgents lists the supported agents whose binary is present on this machine.
func (app *App) detectedAgents(ctx context.Context) []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && a.Installed(ctx) {
			names = append(names, a.Name())
		}
	}
	return names
}

func (app *App) agentDetail(ctx context.Context, name string) string {
	if s, _, ok := app.agents.Surface(name); ok && s.Installed(ctx) {
		return "installed"
	}
	return ""
}

// adapterDisplayNames maps adapter tokens to their display names, in the given order.
func (app *App) adapterDisplayNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if s, _, ok := app.agents.Surface(n); ok {
			out = append(out, s.DisplayName)
		} else {
			out = append(out, n)
		}
	}
	return out
}

// setupHeaderInfo is the column beside the logo: the product, who terma is signed in
// as right now (and where it is pointed), and the working directory. It reads only local
// state — no network — so it is safe to show before signing in.
func (app *App) setupHeaderInfo(cfg *config.Config, p style.Palette) []string {
	info := []string{p.Bold("Terma CLI") + " " + p.Dim("("+app.version+")")}

	switch {
	case cfg.APIKey != "":
		info = append(info, p.Dim("using TERMA_API_KEY"))
	default:
		cred, err := auth.LoadCredential(cfg.ProfileName)
		if err != nil {
			info = append(info, p.Dim("not signed in — setup will sign you in"))
			break
		}
		who := cmp.Or(cred.UserEmail, "signed in")
		org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
		if org != "" {
			who += "  " + p.Dim("("+org+")")
		}
		info = append(info, who)
	}

	if cwd, err := os.Getwd(); err == nil {
		info = append(info, p.Dim(output.TildePath(cwd)))
	}
	return info
}
