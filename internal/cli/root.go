// Package cli is terma's command line: the command tree, run by an App that holds the
// agents this build registers, its version and its flags.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/procinfo"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type globalFlags struct {
	profile   string
	env       string
	apiURL    string
	authURL   string
	appURL    string
	otlpURL   string
	projectID string
	output    string
}

// NewRootCommand builds the whole command tree. It is a constructor rather than a
// package variable so that every test gets a tree of its own, with its own flags and
// output streams.
func (app *App) NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "terma",
		Short: "Attribute AI coding spend to the sessions, files, and commits that produced it",
		Long: `terma connects your coding agents to Terma and stamps the commits they produce.

  terma setup     optional, once per developer — signs you in and records which
                  coding agents you use (an agent's CLI and desktop app are
                  separate choices). No project or telemetry connection.
  terma install   run in each repository — signs you in if setup has not, binds the
                  repo to a Terma project, points your agents at that project per
                  repository, and (offer to) wire the commit and agent hooks. A
                  colleague who clones an already-onboarded repo runs it too: it sets
                  up their own routing without rewriting the committed files.

Then ` + "`terma doctor`" + ` verifies the whole chain end to end and predicts how much
spend will be attributed.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       app.version,
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			app.printUpdateNotice(cmd)
		},
	}

	// Shell completion stays, hidden: the Homebrew cask generates its completion files by
	// running `terma completion <shell>` during install, and fails the install if it
	// cannot. It is not a command a developer needs listed.
	root.CompletionOptions.HiddenDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&app.flags.profile, "profile", "", "configuration profile to use")
	// The environment and endpoint overrides are for Terma's own engineers (and
	// self-hosted deployments). Hidden: users get production and nothing to choose.
	pf.StringVar(&app.flags.env, "env", "", "built-in environment: prod, dev, local")
	pf.StringVar(&app.flags.apiURL, "api-url", "", "Terma data API base URL")
	pf.StringVar(&app.flags.authURL, "auth-url", "", "Terma auth API base URL")
	pf.StringVar(&app.flags.appURL, "app-url", "", "Terma app base URL (used by login)")
	pf.StringVar(&app.flags.otlpURL, "otlp-url", "", "Terma OTLP ingest URL (used by connect)")
	for _, name := range []string{"env", "api-url", "auth-url", "app-url", "otlp-url"} {
		_ = pf.MarkHidden(name)
	}
	pf.StringVarP(&app.flags.projectID, "project", "p", "", "project override for this command (default: current repository's binding)")
	pf.StringVarP(&app.flags.output, "output", "o", "", "output format: table, json, yaml, csv")

	root.AddCommand(
		// Onboarding: the commands the README leads with.
		app.newSetupCommand(),
		app.newInstallCommand(),
		app.newUninstallCommand(),
		app.newNateCommand(),
		app.newDoctorCommand(),
		app.newStatusCommand(),
		// Insight: what the connected agents did, for whom, and what it cost.
		app.newSessionCommand(),
		app.newUsageCommand(),
		app.newPrincipalCommand(), // advanced: hidden from the primary workflow
		// Harness connections (also reachable under the `telemetry` group).
		app.newTelemetryConnectCommand(), // advanced: install configures telemetry normally
		app.newTelemetryDisconnectCommand(),
		app.newHarnessCommand(),
		app.newTelemetryCommand(),
		// Account and configuration.
		app.newLoginCommand(),
		app.newLogoutCommand(),
		app.newWhoamiCommand(),
		app.newProjectCommand(),
		app.newOrgCommand(),
		app.newConfigCommand(),
		// Maintenance.
		app.newUpdateCommand(),
		app.newSpoolCommand(),
		app.newVersionCommand(),
		// Internal: the target of every installed hook shim.
		app.newHookCommand(),
		app.newAgentCommand(),
		// The local OTLP relay (docs/RELAY.md).
		app.newRelayCommand(),
	)
	return root
}

// newHarnessCommand groups the per-harness views: `list` for the static support
// catalog, `status` for what each harness's own config actually says. A bare
// `terma harness` shows the support catalog, the more useful default.
func (app *App) newHarnessCommand() *cobra.Command {
	list := app.newHarnessListCommand()
	cmd := &cobra.Command{
		Use:    "harness",
		Short:  "Show supported coding agents and their connection status",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE:   app.runHarnessList,
	}
	cmd.AddCommand(list, app.newTelemetryStatusCommand())
	return cmd
}

func (app *App) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "version",
		Short:  "Print the terma version",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.loadConfig()
			env := ""
			if err == nil && cfg.Environment != config.EnvProd {
				env = " (" + cfg.Environment + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terma %s%s\n", app.version, env)
			return nil
		},
	}
}

// printUpdateNotice runs daily update maintenance after interactive commands, and the
// first time a new release runs one, the refresh of what earlier versions installed —
// never from a hook or a spool flush (those must stay silent and fast).
func (app *App) printUpdateNotice(cmd *cobra.Command) {
	// nate deliberately removes the updater's state and the running executable. Its
	// post-run must not recreate either half of the installation it just removed.
	if cmd.Name() == "nate" {
		return
	}
	if !app.automaticUpdatesAllowed(cmd, canPrompt()) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	app.refreshAfterUpgrade(cmd.Context(), dir, cmd.ErrOrStderr())
	exe, err := os.Executable()
	if err != nil {
		return
	}
	client := &selfupdate.Client{Version: app.version}
	client.Maintain(cmd.Context(), dir, exe, cmd.ErrOrStderr())
}

func (app *App) automaticUpdatesAllowed(cmd *cobra.Command, interactive bool) bool {
	if !interactive || (app.flags.output != "" && app.flags.output != "table") || os.Getenv("CI") != "" || os.Getenv("TERMA_NO_UPDATE_CHECK") == "1" {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "hook", "spool", "update", "version", "completion", "__complete", "__completeNoDesc":
			return false
		}
	}
	return true
}

// migrateState brings the state an earlier terma left behind up to this build before any
// command reads it (internal/migrate). It runs on every start, hooks included, because
// after an upgrade a hook is as likely as anything to be the new build's first run; when
// nothing is pending it costs one small read. It never fails the command: a hook or a
// launch shim stays silent and gives migrating about a second — the bound covers the
// migrations themselves, which stop between steps and carry on at the next start — and
// anything else says what failed on a terminal a person is watching. Tests do not come through here, so none
// can migrate a developer's real config directory.
func migrateState(ctx context.Context, args []string) {
	dir, err := config.Dir()
	if err != nil || !migrate.Pending(dir) {
		return
	}
	quiet := len(args) > 0 && slices.Contains([]string{"hook", "spool"}, args[0])
	wait := 10 * time.Second
	if quiet {
		wait = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if _, err := migrate.Run(ctx, dir, false); err != nil && !quiet && canPrompt() {
		fmt.Fprintf(os.Stderr, "terma could not update its saved state for this version (%v). Run `terma update --refresh` to retry.\n", err)
	}
}

// App is terma's command line: what every command reads, held for one run.
type App struct {
	// agents are the agents this build knows, and version its release tag ("dev" for a
	// source build).
	agents  *agents.Registry
	version string
	flags   globalFlags
	// binDirs are where doctor looks for other terma builds besides PATH.
	binDirs func() []string
	// hookExecutable is the terma machine-wide hooks and git's global hooks call: this
	// one, by the path it was started as.
	hookExecutable func() (string, error)
	// managedRoot prefixes the system paths managed configuration lives at.
	managedRoot string
	// sessionGetWait bounds `session get`: the gateway serves a single session's
	// roll-up only as a live feed, which has no request timeout.
	sessionGetWait time.Duration
	// runUpdateStep runs one program of an update attached to the terminal.
	runUpdateStep func(ctx context.Context, out io.Writer, argv ...string) error
	// nateBinaryCandidates and nateRemoveBinary find and delete installed termas.
	nateBinaryCandidates func() []string
	nateRemoveBinary     func(cmd *cobra.Command, path string) error
}

// New is the command line for the agents this build knows, at version.
func New(known *agents.Registry, version string) *App {
	app := &App{agents: known, version: version, binDirs: doctor.WellKnownBinDirs, hookExecutable: procinfo.AbsExecutable,
		managedRoot: "/", sessionGetWait: 15 * time.Second, runUpdateStep: runUpdateStep, nateRemoveBinary: removeNateBinary}
	app.nateBinaryCandidates = app.installedTermaBinaries
	return app
}

// Execute runs the command line and returns the process's exit status: 0, 1 for a
// failure, or the code a command chose to mean something more specific (see
// exitcode.go).
func (app *App) Execute() int {
	// The first SIGINT/SIGTERM cancels the running command's context so an in-flight
	// request unwinds promptly instead of waiting out the HTTP timeout. Default signal
	// handling is then restored, so a second signal still force-terminates.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caught := make(chan os.Signal, 1)
	go func() {
		select {
		case s := <-sigCh:
			caught <- s
			cancel()
			signal.Stop(sigCh)
		case <-ctx.Done():
		}
	}()

	migrateState(ctx, os.Args[1:])

	if err := app.NewRootCommand().ExecuteContext(ctx); err != nil {
		// A command that has already explained itself on stdout ends the process
		// with its own code, and prints nothing more.
		if code, ok := exitCodeOf(err); ok {
			return code
		}
		if errors.Is(err, context.Canceled) {
			select {
			case s := <-caught:
				if s == syscall.SIGTERM {
					return 143
				}
				return 130
			default:
			}
		}
		// An error's fix is usually a command to run, so it is drawn as one.
		errOut := style.Highlight(os.Stderr)
		label := style.For(os.Stderr).Fail("Error:")
		if errors.Is(err, auth.ErrNotLoggedIn) {
			fmt.Fprintf(errOut, "%s not signed in. Run `terma setup` (or `terma login`).\n", label)
			return 1
		}
		if wrongEnv, ok := errors.AsType[*auth.ErrWrongEnvironment](err); ok {
			fmt.Fprintf(errOut, "%s %v\n", label, wrongEnv)
			return 1
		}
		fmt.Fprintf(errOut, "%s %v\n", label, err)
		return 1
	}
	return 0
}

func (app *App) loadConfig() (*config.Config, error) {
	return config.Load(config.Overrides{
		Profile:   app.flags.profile,
		Env:       app.flags.env,
		APIURL:    app.flags.apiURL,
		AuthURL:   app.flags.authURL,
		AppURL:    app.flags.appURL,
		OTLPURL:   app.flags.otlpURL,
		ProjectID: app.flags.projectID,
	})
}

func (app *App) resolveFormat() (output.Format, error) {
	return output.Resolve(app.flags.output)
}

func (app *App) loadProjectConfig() (*config.Config, error) {
	cfg, err := app.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := resolveRepoProject(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveRepoProject supplies the repository's project only when the caller has
// not given a command/env override. Git's worktree root prevents a nested checkout
// from inheriting the parent repository's binding; a linked worktree without one of its
// own uses its main checkout's (project.Resolve). No profile is changed.
func resolveRepoProject(cfg *config.Config) error {
	if cfg.ProjectID != "" {
		return nil
	}
	root, gitDir, err := workspaceHere(context.Background())
	if err != nil {
		return nil // Outside a repository: requireProject explains the next step.
	}
	bound, _, err := termaproject.Resolve(root, gitDir)
	if errors.Is(err, termaproject.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg.ProjectID, cfg.ProjectName = bound.Project.ID, bound.Project.Name
	cfg.ProjectOrganizationID = bound.Project.OrganizationID
	return nil
}

// newClient builds an authenticated client. Commands that read project-scoped data
// call requireProject first so the missing-project case is a clear local message
// rather than a 400 from the gateway.
func (app *App) newClient(cfg *config.Config) (*api.Client, error) {
	return api.New(cfg, api.Options{Version: app.version, ProjectID: cfg.ProjectID})
}

func requireProject(cfg *config.Config) error {
	if err := resolveRepoProject(cfg); err != nil {
		return err
	}
	if cfg.APIKey != "" || cfg.ProjectID != "" {
		return nil
	}
	return errors.New("no project bound to this repository — run `terma install` inside a repository, or pass --project for this command")
}

// repoHere locates the repository the CLI runs in: its worktree root and its git
// directory. outside is the caller's own sentence for a directory that is in no
// repository ("terma install runs inside a git repository"); left empty, git's own
// error comes back, for a caller that wraps it or only needs to know.
func repoHere(ctx context.Context, outside string) (root, gitDir string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	root, gitDir, err = gitx.Locate(ctx, cwd)
	if err != nil && outside != "" {
		return "", "", errors.New(outside)
	}
	return root, gitDir, err
}

func workspaceHere(ctx context.Context) (root, gitDir string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	return termaproject.Locate(ctx, cwd)
}

// setupCommand is the preamble every signed-in read shares: the configuration, the
// output format and a client. A command's own preconditions run on the configuration
// before the format or the credential is looked at, so a missing project is reported
// ahead of a sign-in error rather than hidden behind it.
func (app *App) setupCommand(preconditions ...func(*config.Config) error) (*config.Config, *api.Client, output.Format, error) {
	cfg, err := app.loadConfig()
	if err != nil {
		return nil, nil, "", err
	}
	for _, check := range preconditions {
		if err := check(cfg); err != nil {
			return nil, nil, "", err
		}
	}
	format, err := app.resolveFormat()
	if err != nil {
		return nil, nil, "", err
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	return cfg, client, format, nil
}

// setupProjectCommand is the preamble every project-scoped read shares.
func (app *App) setupProjectCommand(cmd *cobra.Command) (context.Context, *api.Client, output.Format, error) {
	_, client, format, err := app.setupCommand(requireProject)
	if err != nil {
		return nil, nil, "", err
	}
	return cmd.Context(), client, format, nil
}
