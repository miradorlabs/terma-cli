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
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/globalmode"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/setup"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
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
	// desktopGrant is the hidden --desktop-grant: Terma Desktop signs this CLI in over
	// stdin and stdout (signInWithGrant), for the --org and --team it names.
	desktopGrant bool
}

func (app *App) newSetupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use: "setup",
		// login was its own command; onboarding emails already sent say `terma login`.
		Aliases: []string{"login"},
		Short:   "Sign in, choose your team and coding agents, and write machine-wide hooks",
		Long: `Gets this machine ready to use terma, once per developer:

  1. Signs you in with one approval in the browser (--no-browser prints the URL
     instead), to every organization you tick there. The active one is --org's,
     else the one you had, else your personal one. A working sign-in is reused,
     asking which organization when you belong to several.
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
	// Hidden like auth and hook: Terma Desktop runs it, people do not.
	cmd.Flags().BoolVar(&f.desktopGrant, "desktop-grant", false, "sign in with a code Terma Desktop writes on stdin (with -y, --org and --team ids)")
	_ = cmd.Flags().MarkHidden("desktop-grant")
	return cmd
}

func (app *App) runSetup(cmd *cobra.Command, f setupFlags) error {
	out := cmd.OutOrStdout()
	if f.desktopGrant {
		if err := app.checkDesktopGrant(f); err != nil {
			return err
		}
	}
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
	if f.desktopGrant && cfg.APIKey != "" {
		return errors.New("TERMA_API_KEY is set — unset it for Terma Desktop to sign in as the developer")
	}
	// Under --desktop-grant, stdout's first line is the grant's: the header goes to stderr.
	headerOut := out
	if f.desktopGrant {
		headerOut = cmd.ErrOrStderr()
	}
	if p := style.For(headerOut); p.Enabled() {
		fmt.Fprintln(headerOut, style.Header(p, app.setupHeaderInfo(cfg, p)))
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
	// Asked before setup writes anything: a machine never set up before moves to routing
	// by teams with the relay setup starts.
	fresh := app.freshInstall()
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
			var kept int
			var err error
			if f.desktopGrant {
				cfg, err = app.signInWithGrant(cmd, cfg, parseOrgRef(f.org).ID)
			} else {
				opts := signInOptions{org: parseOrgRef(f.org), noBrowser: f.noBrowser, pauseBeforeBrowser: !f.assumeYes}
				cfg, kept, err = app.setupSignIn(cmd, cfg, opts, askOrganization(cmd, f.assumeYes))
			}
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
			if fresh {
				// Before the relay starts, so its start migrates (migrate.OptIn).
				if err := migrate.OptIn(app.stateDir); err != nil {
					return err
				}
			}
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
	} else if config.Migrated(app.dir) {
		steps.ChooseTeams = app.setupTeams(cmd, ui, f)
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
	if failure, ok := errors.AsType[*auth.GrantError](err); ok && f.desktopGrant {
		return writeGrantFailure(out, cmd.ErrOrStderr(), failure)
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

// freshInstall reports whether terma was never set up on this machine: config.json names
// no profile, and the machine has not moved to routing by teams. Such a machine has no
// collection of its own to keep, so its setup opts it in to routing by teams
// (migrate.OptIn); one set up before moves only with `terma teams`.
func (app *App) freshInstall() bool {
	file, err := config.LoadFile(app.dir)
	return err == nil && len(file.Profiles) == 0 && !config.Migrated(app.dir)
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
	ui.Summary("Updates", updatesSummary(selfupdate.InstallOf(exe, app.version), p.Auto))
}
