// Package cmd wires the terma command tree.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// Version is stamped at build time via -ldflags: the release tag by GoReleaser,
// `git describe` by `make build`. "dev" is the unset sentinel.
var Version = "dev"

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

var flags globalFlags

// NewRootCommand builds the whole command tree. It is a constructor rather than a
// package variable so that every test gets a tree of its own, with its own flags and
// output streams.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "terma",
		Short: "Attribute AI coding spend to the sessions, files, and commits that produced it",
		Long: `terma connects your coding agents to Terma and stamps the commits they produce.

  terma setup     optional, once per developer — signs you in and records which
                  coding agents you use (including Codex CLI and Codex Desktop
                  as separate choices). No project or telemetry connection.
  terma install   run in each repository — signs you in if setup has not, binds the
                  repo to a Terma project, points your agents at that project per
                  repository, and (offer to) wire the commit and agent hooks. A
                  colleague who clones an already-onboarded repo runs it too: it sets
                  up their own routing without rewriting the committed files.

Then ` + "`terma doctor`" + ` verifies the whole chain end to end and predicts how much
spend will be attributed.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			printUpdateNotice(cmd)
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flags.profile, "profile", "", "configuration profile to use")
	// The environment and endpoint overrides are for Terma's own engineers (and
	// self-hosted deployments). Hidden: users get production and nothing to choose.
	pf.StringVar(&flags.env, "env", "", "built-in environment: prod, dev, local")
	pf.StringVar(&flags.apiURL, "api-url", "", "Terma data API base URL")
	pf.StringVar(&flags.authURL, "auth-url", "", "Terma auth API base URL")
	pf.StringVar(&flags.appURL, "app-url", "", "Terma app base URL (used by login)")
	pf.StringVar(&flags.otlpURL, "otlp-url", "", "Terma OTLP ingest URL (used by connect)")
	for _, name := range []string{"env", "api-url", "auth-url", "app-url", "otlp-url"} {
		_ = pf.MarkHidden(name)
	}
	pf.StringVarP(&flags.projectID, "project", "p", "", "project override for this command (default: current repository's binding)")
	pf.StringVarP(&flags.output, "output", "o", "", "output format: table, json, yaml, csv")

	root.AddCommand(
		// Onboarding: the commands the README leads with.
		newSetupCommand(),
		newInstallCommand(),
		newUninstallCommand(),
		newDoctorCommand(),
		newStatusCommand(),
		// Insight: what the connected agents did, for whom, and what it cost.
		newSessionCommand(),
		newUsageCommand(),
		newPrincipalCommand(), // advanced: hidden from the primary workflow
		newBlameCommand(),
		// Harness connections (also reachable under the `telemetry` group).
		newTelemetryConnectCommand(), // advanced: install configures telemetry normally
		newTelemetryDisconnectCommand(),
		newHarnessCommand(),
		newTelemetryCommand(),
		// Account and configuration.
		newLoginCommand(),
		newLogoutCommand(),
		newWhoamiCommand(),
		newProjectCommand(),
		newOrgCommand(),
		newConfigCommand(),
		// Maintenance.
		newUpdateCommand(),
		newSpoolCommand(),
		newVersionCommand(),
		// Internal: the target of every installed hook shim.
		newHookCommand(),
		// Internal: the target of the per-repo routing PATH shim / wrapper.
		newShimCommand(),
		newDesktopCommand(),
	)
	return root
}

// newHarnessCommand groups the per-harness views: `list` for the static support
// catalog, `status` for what each harness's own config actually says. A bare
// `terma harness` shows the support catalog, the more useful default.
func newHarnessCommand() *cobra.Command {
	list := newHarnessListCommand()
	cmd := &cobra.Command{
		Use:    "harness",
		Short:  "Show supported coding agents and their connection status",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE:   runHarnessList,
	}
	cmd.AddCommand(list, newTelemetryStatusCommand())
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "version",
		Short:  "Print the terma version",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			env := ""
			if err == nil && cfg.Environment != config.EnvProd {
				env = " (" + cfg.Environment + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terma %s%s\n", Version, env)
			return nil
		},
	}
}

// printUpdateNotice runs daily update maintenance after interactive commands,
// never from a hook or a spool flush (those must stay silent and fast).
func printUpdateNotice(cmd *cobra.Command) {
	if !automaticUpdatesAllowed(cmd, canPrompt()) {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	client := &selfupdate.Client{Version: Version}
	client.Maintain(cmd.Context(), dir, exe, cmd.ErrOrStderr())
}

func automaticUpdatesAllowed(cmd *cobra.Command, interactive bool) bool {
	if !interactive || (flags.output != "" && flags.output != "table") || os.Getenv("CI") != "" || os.Getenv("TERMA_NO_UPDATE_CHECK") == "1" {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "hook", "shim", "spool", "update", "version", "completion", "__complete", "__completeNoDesc":
			return false
		}
	}
	return true
}

// Execute runs the command line and returns the process's exit status: 0, 1 for a
// failure, or the code a command chose to mean something more specific (see
// exitcode.go).
func Execute() int {
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

	if err := NewRootCommand().ExecuteContext(ctx); err != nil {
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
		label := style.For(os.Stderr).Fail("Error:")
		if errors.Is(err, auth.ErrNotLoggedIn) {
			fmt.Fprintf(os.Stderr, "%s not signed in. Run `terma setup` (or `terma login`).\n", label)
			return 1
		}
		if wrongEnv, ok := errors.AsType[*auth.ErrWrongEnvironment](err); ok {
			fmt.Fprintf(os.Stderr, "%s %v\n", label, wrongEnv)
			return 1
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", label, err)
		return 1
	}
	return 0
}

func loadConfig() (*config.Config, error) {
	return config.Load(config.Overrides{
		Profile:   flags.profile,
		Env:       flags.env,
		APIURL:    flags.apiURL,
		AuthURL:   flags.authURL,
		AppURL:    flags.appURL,
		OTLPURL:   flags.otlpURL,
		ProjectID: flags.projectID,
	})
}

func resolveFormat() (output.Format, error) {
	return output.Resolve(flags.output)
}

func loadProjectConfig() (*config.Config, error) {
	cfg, err := loadConfig()
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
// from inheriting the parent repository's binding. No profile is changed.
func resolveRepoProject(cfg *config.Config) error {
	if cfg.ProjectID != "" {
		return nil
	}
	root, _, err := repoHere(context.Background(), "")
	if err != nil {
		return nil // Outside a repository: requireProject explains the next step.
	}
	bound, err := termaproject.Load(root)
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
func newClient(cfg *config.Config) (*api.Client, error) {
	return api.New(cfg, api.Options{Version: Version, ProjectID: cfg.ProjectID})
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

// setupCommand is the preamble every signed-in read shares: the configuration, the
// output format and a client. A command's own preconditions run on the configuration
// before the format or the credential is looked at, so a missing project is reported
// ahead of a sign-in error rather than hidden behind it.
func setupCommand(preconditions ...func(*config.Config) error) (*config.Config, *api.Client, output.Format, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, "", err
	}
	for _, check := range preconditions {
		if err := check(cfg); err != nil {
			return nil, nil, "", err
		}
	}
	format, err := resolveFormat()
	if err != nil {
		return nil, nil, "", err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	return cfg, client, format, nil
}

// setupProjectCommand is the preamble every project-scoped read shares.
func setupProjectCommand(cmd *cobra.Command) (context.Context, *api.Client, output.Format, error) {
	_, client, format, err := setupCommand(requireProject)
	if err != nil {
		return nil, nil, "", err
	}
	return cmd.Context(), client, format, nil
}
