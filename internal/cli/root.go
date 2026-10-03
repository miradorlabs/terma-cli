// Package cli is terma's command line: the command tree, run by an App that holds the
// agents this build registers, its version and its flags.
package cli

import (
	"cmp"
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

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
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

// NewRootCommand builds a fresh command tree, so every test gets its own flags and streams.
func (app *App) NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "terma",
		Short: "Attribute AI coding spend to the sessions, files, and commits that produced it",
		Long: `terma connects your coding agents to Terma and stamps the commits they produce.

  terma setup      once per developer: signs you in, chooses your team and coding
                   agents, and writes machine-wide hooks. Your team's policy lists
                   the folders it collects; nothing is written into a repository's
                   files.
                   Run it again to switch team or organization, or repair the machine.
  terma doctor     verifies the whole chain end to end; every failure names its fix.
  terma update     installs the latest release and refreshes what terma installed.
  terma teardown   undoes setup on this machine (--sign-out also signs out).

Every command is safe to run again.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       app.version,
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			app.printUpdateNotice(cmd)
		},
	}

	// Hidden, not removed: the Homebrew cask runs `terma completion <shell>` during install.
	root.CompletionOptions.HiddenDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&app.flags.profile, "profile", "", "configuration profile to use")
	// Environment and endpoint overrides are for Terma's own engineers.
	pf.StringVar(&app.flags.env, "env", "", "built-in environment: prod, dev, local")
	pf.StringVar(&app.flags.apiURL, "api-url", "", "Terma data API base URL")
	pf.StringVar(&app.flags.authURL, "auth-url", "", "Terma auth API base URL")
	pf.StringVar(&app.flags.appURL, "app-url", "", "Terma app base URL (used by login)")
	pf.StringVar(&app.flags.otlpURL, "otlp-url", "", "Terma OTLP ingest URL")
	for _, name := range []string{"env", "api-url", "auth-url", "app-url", "otlp-url"} {
		_ = pf.MarkHidden(name)
	}
	pf.StringVarP(&app.flags.projectID, "team", "t", "", "team override for this command (default: the team chosen at setup)")
	pf.StringVarP(&app.flags.output, "output", "o", "", "output format: table, json, yaml, csv")

	root.AddCommand(
		app.newSetupCommand(),
		app.newDoctorCommand(),
		app.newUpdateCommand(),
		app.newTeardownCommand(),
		// Hidden. Other programs run these: agents and git run hook, the relay service runs
		// relay, hooks run spool, the Homebrew cask runs completion and install.sh version.
		app.newHookCommand(),
		app.newRelayCommand(),
		app.newSpoolCommand(),
		app.newVersionCommand(),
		// Hidden, for Terma's engineers: config switches deployments, nate wipes a machine.
		app.newConfigCommand(),
		app.newNateCommand(),
		// Hidden until the e2e suites stop calling it (MIR-80); setup and doctor do its job.
		app.newStatusCommand(),
	)
	return root
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

// printUpdateNotice runs update maintenance after interactive commands, never from a
// hook or a spool flush, which must stay silent and fast.
func (app *App) printUpdateNotice(cmd *cobra.Command) {
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
		// nate removes the updater's state and the executable; its post-run must not recreate them.
		case "hook", "spool", "update", "version", "completion", "__complete", "__completeNoDesc", "nate":
			return false
		}
	}
	return true
}

// migrateState runs pending migrations before any command, hooks included, since a hook
// may be a new build's first run; hooks get about a second and never fail.
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
		fmt.Fprintf(os.Stderr, "terma could not update its saved state for this version (%v). Run `terma update` to retry.\n", err)
	}
}

// App is terma's command line: what every command reads, held for one run.
type App struct {
	agents  *agents.Registry
	version string
	flags   globalFlags
	binDirs func() []string
	// hookExecutable is the terma machine-wide and global git hooks call, by its start path.
	hookExecutable       func() (string, error)
	managedRoot          string
	runUpdateStep        func(ctx context.Context, out io.Writer, argv ...string) error
	nateBinaryCandidates func() []string
	nateRemoveBinary     func(cmd *cobra.Command, path string) error
}

// New is the command line for the agents this build knows, at version.
func New(known *agents.Registry, version string) *App {
	app := &App{agents: known, version: version, binDirs: doctor.WellKnownBinDirs, hookExecutable: procinfo.AbsExecutable,
		managedRoot: "/", runUpdateStep: runUpdateStep, nateRemoveBinary: removeNateBinary}
	app.nateBinaryCandidates = app.installedTermaBinaries
	return app
}

// Execute runs the command line and returns the process's exit status (see exitcode.go).
func (app *App) Execute() int {
	// The first signal cancels the context; a second one force-terminates.
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
		// A command that already explained itself on stdout ends with its own code.
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
		errOut := style.Highlight(os.Stderr)
		label := style.For(os.Stderr).Fail("Error:")
		if errors.Is(err, auth.ErrNotLoggedIn) {
			fmt.Fprintf(errOut, "%s not signed in. Run `terma setup`.\n", label)
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
	cfg.ProjectID = cmp.Or(cfg.ProjectID, cfg.Policy.TeamID)
	return cfg, nil
}

func (app *App) newClient(cfg *config.Config) (*api.Client, error) {
	return api.New(cfg, api.Options{Version: app.version, ProjectID: cfg.ProjectID})
}

func workspaceHere(ctx context.Context) (root, gitDir string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	return termaproject.Locate(ctx, cwd)
}
