package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/setup"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type setupFlags struct {
	harnesses string
	// org is --org: the organization to sign into, by name or id; "" keeps the current one.
	org       string
	noBrowser bool
	assumeYes bool
	// relayService is --relay-service: "on", "off", or "" to keep the recorded choice.
	relayService string
	// relayAddr is --relay-addr: a loopback address to move the relay to, "" to keep it.
	relayAddr string
	// managedConfig is --managed-config's directory for global mode's managed hooks, which
	// call terma at managedTerma.
	managedConfig string
	managedTerma  string
	verbose       bool
}

// agentChoice is an onboarding surface: one repository adapter may be offered as several
// launch surfaces a developer picks independently.
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
		Use: "setup",
		// login was its own command; onboarding emails already sent say `terma login`.
		Aliases: []string{"login"},
		Short:   "Sign in and choose your coding agents (once per developer)",
		Long: `Gets this machine ready to use terma, once per developer:

  1. Signs you in (a browser handoff; --no-browser prints the URL instead).
  2. Records which coding agents you work with (an agent's CLI and desktop app
     separately).
  3. Fetches your organization's collection policy.
  4. Points those agents' telemetry at terma's local relay, and runs the relay in
     the background (--relay-service off: started on demand instead). Only sessions
     allowed by the selected team's collection policy leave this machine.

A repository your organization connected in Terma needs nothing more: its committed
hooks claim its sessions. ` + "`terma install`" + ` connects a repository from here instead.

Run it again any time: it reuses a working sign-in, --org switches organization, and
--relay-addr moves the relay off a port another program holds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runSetup(cmd, f) },
	}
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to record ("+strings.Join(app.availableAgentNames(), ", ")+"); default: a picker")
	cmd.Flags().StringVar(&f.org, "org", "", "organization to sign into, by name or id (default: the current one)")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "skip the browser prompt and picker; record every available installed agent")
	cmd.Flags().StringVar(&f.relayService, "relay-service", "", "run the local relay as a background service: on or off (default: on, or your last choice)")
	cmd.Flags().StringVar(&f.relayAddr, "relay-addr", "", "move the local relay to this loopback address (default "+claim.DefaultAddr+", or the one recorded)")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "show each step and what it wrote")
	cmd.Flags().StringVar(&f.managedConfig, "managed-config", "", "write global mode's hooks as managed configuration into this directory, for your organization to deploy, and exit")
	cmd.Flags().StringVar(&f.managedTerma, "managed-terma", "$HOME/.local/bin/terma", "with --managed-config: where terma is installed on the machines")
	return cmd
}

func (app *App) runSetup(cmd *cobra.Command, f setupFlags) error {
	out := cmd.OutOrStdout()
	if f.managedConfig != "" {
		files, err := globalmode.WriteManaged(app.agents, f.managedConfig, f.managedTerma)
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
	if p := style.For(out); p.Enabled() {
		fmt.Fprintln(out, style.Header(p, app.setupHeaderInfo(cfg, p)))
	}
	if cfg.APIKey != "" {
		return setup.ErrServerKey
	}
	switch f.relayService {
	case "", "on", "off":
	default:
		return fmt.Errorf("--relay-service %q: want on or off", f.relayService)
	}
	if f.relayAddr != "" {
		if err := checkRelayAddr(f.relayAddr); err != nil {
			return err
		}
	}

	ui := newInstallUI(out, f.verbose)
	ui.title, ui.warnTitle = "Setup complete", "Almost done — finish the steps marked ! below"
	recorded := false
	team := ""
	res, err := setup.Run(cmd.Context(), app.agents, cfg, setup.Steps{
		SignIn: func(_ context.Context, cfg *config.Config) (*config.Config, error) {
			cfg, err := app.signInAndReload(cmd, cfg, signInOptions{org: parseOrgRef(f.org), noBrowser: f.noBrowser, pauseBeforeBrowser: !f.assumeYes})
			if err == nil {
				fmt.Fprintln(out)
				ui.Summary("Signed in", signedInAs(cfg))
			}
			return cfg, err
		},
		// A machine-level preference, not a connection.
		ChooseAgents: func(_ context.Context, cfg *config.Config) ([]string, error) {
			return app.chooseHarnesses(cmd, cfg, f)
		},
		Recorded: func(names []string) {
			recorded = true
			if len(names) == 0 {
				ui.Warn("Agents", "none chosen")
				ui.Then("Run `terma setup` again to choose your coding agents.")
			} else {
				ui.Summary("Agents", strings.Join(app.adapterDisplayNames(names), ", "))
			}
		},
		SelectTeam: func(_ context.Context, cfg *config.Config) error {
			name, err := app.selectPolicyTeam(cmd, cfg)
			team = name
			return err
		},
		FetchPolicy: app.policies().Fetch,
		StopRelay: func() {
			if dir, err := daemon.Dir(); err == nil {
				daemon.Stop(dir)
			}
		},
		Fetched: func(pol config.Policy) {
			if team != "" {
				ui.Summary("Team", team)
			}
			ui.Summary("Collects", doctor.PolicySummary(pol))
		},
		ConnectRelay: func(ctx context.Context, names []string) error {
			if f.relayAddr != "" {
				if err := moveRelay(f.relayAddr); err != nil {
					return err
				}
			}
			return app.connectMachineRelay(ctx, names, f.relayService, relayReport{
				ok: func(label, what string) {
					if label == "Relay" {
						ui.Summary(label, what)
					} else {
						ui.OK(label, what)
					}
				},
				warn: func(label, what string) {
					ui.Warn(label, what)
					ui.Then("Run `terma doctor` to see what is wrong and how to fix it.")
				},
				then:   ui.Then,
				detail: ui.Detail(),
			})
		},
		SpoolKey: func(ctx context.Context, cfg *config.Config) {
			if k := app.ensureSpoolKey(ctx, cfg); k.fix == "" {
				ui.OK("Hook events", k.state)
			} else {
				ui.Warn("Hook events", k.state)
				ui.Then(k.fix)
			}
		},
		MachineHooks: func(ctx context.Context, names []string) error {
			return app.globalMode().Apply(ctx, names, func(what string) { ui.OK("Machine", what) },
				func(what string) { ui.Summary("Machine", what) }, ui.Then)
		},
		CheckIn: func(ctx context.Context) {
			if ok, what := daemon.CheckIn(ctx); ok {
				ui.Summary("Check-in", what)
			} else {
				ui.Warn("Check-in", what)
				ui.Then("Run `terma doctor` to see why this machine could not report to your organization.")
			}
		},
	})
	// A team picker left after the agents were recorded is still a cancellation, but not
	// one that recorded nothing.
	if errors.Is(err, errCancelled) {
		if recorded {
			fmt.Fprintln(out, "Cancelled before a team was selected; the agents above stay recorded.")
		} else {
			fmt.Fprintln(out, "Cancelled. Nothing was recorded.")
		}
		return nil
	}
	if err != nil {
		ui.printLines()
		return err
	}
	if res.Policy.Global() {
		ui.title = "Setup complete — every session and commit on this machine reports to " + cmp.Or(team, "your organization")
	}
	ui.Then("Run `terma doctor` any time to see what terma is collecting and check it end to end.")
	ui.finish()
	return nil
}

// signedInAs is who the sign-in left this profile as: the email and organization.
func signedInAs(cfg *config.Config) string {
	who := "signed in"
	if cred, err := auth.LoadCredential(cfg.ProfileName); err == nil && cred.UserEmail != "" {
		who = cred.UserEmail
	}
	if org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID); org != "" {
		who += " (" + org + ")"
	}
	return who
}

// selectPolicyTeam picks the team whose policy is set up, and returns its name; it never
// creates a telemetry key.
func (app *App) selectPolicyTeam(cmd *cobra.Command, cfg *config.Config) (string, error) {
	if config.PolicyStub() != "" {
		return "", nil
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = cfg.Policy.TeamID
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return "", err
	}
	projects, err := availableProjects(cmd.Context(), client)
	if err != nil {
		return "", err
	}
	var team *project
	if cfg.ProjectID != "" {
		team, err = matchProject(projects, cfg.ProjectID)
	} else {
		team, err = soleOrPick(cmd, projects, "")
	}
	if err != nil {
		return "", err
	}
	cfg.ProjectID = team.ID
	return cmp.Or(team.Name, team.ID), nil
}

// chooseHarnesses resolves the machine-level agent list: --harness, else a picker,
// else (with --yes or no terminal) the available recorded or detected agents.
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

	fmt.Fprintln(cmd.OutOrStdout())
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

// harnessSelectionAgents puts available agents first in registry order; the form and its
// result mapping must share this order.
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

func (app *App) selectedInRegistryOrder(chosen map[string]bool) []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && chosen[a.Name()] {
			names = append(names, a.Name())
		}
	}
	return names
}

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

// setupHeaderInfo is the column beside the logo; it reads only local state, so it is safe
// to show before signing in.
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
