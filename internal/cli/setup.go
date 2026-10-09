package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/setup"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type setupFlags struct {
	harnesses string
	// org is --org: the organization to sign into, by name or id; "" asks among several.
	org       string
	noBrowser bool
	assumeYes bool
	// relayService is --relay-service: "on", "off", or "" to keep the recorded choice.
	relayService string
	// relayAddr is --relay-addr: a loopback address to move the relay to, "" to keep it.
	relayAddr string
	// managedConfig is --managed-config's directory for the managed hooks, which
	// call terma at managedTerma.
	managedConfig string
	managedTerma  string
	verbose       bool
	// insecureStorage is --insecure-storage: credentials in plain-text files, not the keychain.
	insecureStorage bool
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
		Short:   "Sign in, choose your team and coding agents, and write machine-wide hooks",
		Long: `Gets this machine ready to use terma, once per developer:

  1. Signs you in (a browser handoff; --no-browser prints the URL instead) and, when
     you belong to several organizations, asks which one (--org names it).
  2. Records which coding agents you work with.
  3. Chooses your team (--team names it) and fetches its collection policy, which
     lists the repositories it collects.
  4. Points those agents' telemetry at terma's local relay, and runs the relay in
     the background (--relay-service off: started on demand instead).
  5. Writes the agents' machine-wide hooks, so a session in a repository the
     policy lists is collected for your team, and nothing anywhere else. git's
     configuration is not touched: where the policy asks for commit stamping, the
     first agent session in such a repository installs three hooks in its own
     .git/hooks, chaining to any hook already there. Nothing is written into a
     repository's working tree or committed files.

Run it again any time: it reuses a working sign-in, offers the team chosen before
as the default when the organization has several, --team switches team, --org
switches organization, and --relay-addr moves the relay off a port another program
holds.

With TERMA_API_KEY set to a team server key (Ingest permission), setup signs in with
the key instead, with no browser: it sets up the key's own team, so --org and --team
take only that organization's and team's ids. Nothing else needs TERMA_API_KEY
afterwards, but setup itself does: run it again with the key set, or with a new key to
rotate it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runSetup(cmd, f) },
	}
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to record ("+strings.Join(app.availableAgentNames(), ", ")+"); default: a picker")
	cmd.Flags().StringVar(&f.org, "org", "", "organization to sign into, by name or id (default: asks when you belong to several)")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "skip the browser prompt and picker; record every available installed agent")
	cmd.Flags().StringVar(&f.relayService, "relay-service", "", "run the local relay as a background service: on or off (default: on, or your last choice)")
	cmd.Flags().StringVar(&f.relayAddr, "relay-addr", "", "move the local relay to this loopback address (default "+claim.DefaultAddr+", or the one recorded)")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "show each step and what it wrote")
	cmd.Flags().BoolVar(&f.insecureStorage, "insecure-storage", false, "save credentials in plain text instead of the system keychain")
	cmd.Flags().StringVar(&f.managedConfig, "managed-config", "", "write the machine-wide hooks as managed configuration into this directory, for your organization to deploy, and exit")
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
	// Sign-in places secrets by it, and later writes follow; a failed sign-in restores it. A
	// server key's setup records it only when it keeps the key (keepServerKey).
	wasInsecure := config.InsecureStorage(app.dir)
	if cfg.APIKey == "" {
		if err := config.UpdateFile(app.dir, func(file *config.File) { file.InsecureStorage = f.insecureStorage }); err != nil {
			return err
		}
	}

	ui := newSetupUI(out, f.verbose)
	ui.title, ui.warnTitle = "Setup complete", "Almost done — finish the steps marked ! below"
	recorded := false
	team := ""
	steps := setup.Steps{
		SignIn: func(_ context.Context, cfg *config.Config) (*config.Config, error) {
			opts := signInOptions{org: parseOrgRef(f.org), noBrowser: f.noBrowser, pauseBeforeBrowser: !f.assumeYes}
			cfg, kept, err := app.setupSignIn(cmd, cfg, opts, askOrganization(cmd, f.assumeYes))
			if err == nil {
				fmt.Fprintln(out)
				ui.Summary("Signed in", signedInAs(cfg))
				if kept > 1 {
					ui.Caution("Organization", fmt.Sprintf("kept %s, one of your %d: `terma setup --org <name>` sets up another",
						cmp.Or(cfg.OrganizationName, cfg.OrganizationID), kept))
				}
				reportCredentialStore(ui, cfg, f.insecureStorage)
				app.settleSecrets(ui, cfg, f.insecureStorage)
			} else {
				// A stored session it reused was already saved under the storage being tried.
				_ = config.UpdateFile(app.dir, func(file *config.File) { file.InsecureStorage = wasInsecure })
				_ = auth.Relocate(app.dir)
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
			name, err := app.selectPolicyTeam(cmd, cfg, !f.assumeYes && canPrompt())
			team = name
			return err
		},
		FetchPolicy: app.policies().Fetch,
		StopRelay: func() {
			if dir, err := daemon.Dir(app.stateDir); err == nil {
				daemon.Stop(dir)
			}
		},
		Fetched: func(pol config.Policy) {
			ui.policyFetched(team, pol)
			app.recordTeamName(cfg.ProfileName, pol.TeamID, team)
		},
		ConnectRelay: func(ctx context.Context, names []string) error {
			if f.relayAddr != "" {
				if err := app.moveRelay(f.relayAddr); err != nil {
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
		MachineHooks: func(_ context.Context, names []string) error {
			err := app.globalMode().Apply(names, func(what string) { ui.OK("Machine", what) },
				func(what string) { ui.Summary("Machine", what) }, ui.Then)
			if err == nil {
				app.setupStatusLine(cmd.ErrOrStderr(), ui, names)
			}
			return err
		},
		CheckIn: func(ctx context.Context) {
			if ok, what := daemon.CheckIn(ctx, app.stateDir); ok {
				ui.Summary("Check-in", what)
			} else {
				ui.Warn("Check-in", what)
				ui.Then("Run `terma doctor` to see why this machine could not report to your organization.")
			}
		},
	}
	if cfg.APIKey != "" {
		app.useServerKey(&steps, ui, parseOrgRef(f.org), &team, f.insecureStorage)
	}
	res, err := setup.Run(cmd.Context(), app.agents, cfg, steps)
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
	app.reportUpdates(ui)
	ui.Then("Run `terma doctor` any time to see what terma is collecting and check it end to end.")
	ui.finish()
	return nil
}

// reportUpdates says how this installation gets new releases.
func (app *App) reportUpdates(ui *setupUI) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := selfupdate.LoadPreferences(app.dir)
	if err != nil {
		return
	}
	ui.Summary("Updates", updatesSummary(exe, app.version, runtime.GOOS, p.Auto))
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
