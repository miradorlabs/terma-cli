package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type installFlags struct {
	projectRef   string
	harnesses    string
	adapters     string
	noHooks      bool
	noStatusLine bool
	// relayService is "on", "off", or "" to keep the last choice.
	relayService string
	identity     string
	signals      string
	updatePolicy bool
	noBrowser    bool
	dryRun       bool
	assumeYes    bool
	force        bool
	verbose      bool
}

func (app *App) newInstallCommand() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Configure this workspace: point your agents at its team and wire the hooks",
		Long: `Run once per repository. install is self-contained — it signs you in if you have
not run ` + "`terma setup`" + `, asks which agents you use if you have not chosen, then:

  1. Binds the repository to a Terma team and records it in .terma/settings.json —
     committed, no secrets. A repository already bound keeps its team; otherwise it
     takes the team you chose at setup, or the only one your organization has, and
     asks only when neither decides. --team names another. A binding to a team your
     account cannot see is not used: install says why and chooses again.
  2. Points each of your agents at that team, through the local relay: their own
     exporters (or terma's plugin, for an agent without a usable one) send to a relay
     on this machine, and the relay forwards only the sessions this repository's hooks
     claim, with the team's key. Nothing else leaves the machine. Keys stay in your home
     directory, namespaced by team — never in the repository. Agents send prompt text,
     model responses and tool content to the relay; what of it leaves is your team's
     collection policy's call, set in Terma, never the install's. Excluded paths
     withhold a tool call that names one, in a path field or as a word of a shell
     command; a file read indirectly, by a script the command runs, is not caught. A
     desktop app also reports through repository hooks.
  3. Enables repository telemetry, including for machines configured to export only
     from installed repositories. Existing repository policies are preserved unless
     --signals changes them; a content switch an earlier terma wrote there is removed.
  4. Offers to install the commit hooks and the agents' own hooks (session start/end,
     tool use, stop) into the files you commit, so one merged PR onboards everyone.

Run from any subdirectory of a Git worktree; install uses its root. Outside Git,
the first install uses the current directory and later installs find the nearest
.terma/settings.json above it. Agent hooks and telemetry work there; Git hooks and
commit stamping are skipped. Recognizable non-Git repositories produce a warning:
only Git has version-control integration. Bare repositories are not workspaces.

The keys and per-team configuration live in your home directory; the committed
.terma/settings.json only names the project.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.updatePolicy = cmd.Flags().Changed("signals")
			return app.runInstall(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.projectRef, "team", "", "Terma team (name or id) to bind the repository to")
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to configure ("+strings.Join(app.availableAgentNames(), ", ")+"); default: what `terma setup` recorded, else a picker")
	cmd.Flags().StringVar(&f.adapters, "adapters", "", "comma-separated agents whose committed hooks to wire (default: the configured agents that have one)")
	cmd.Flags().BoolVar(&f.noHooks, "no-hooks", false, "do not install commit hooks or agent hooks")
	cmd.Flags().StringVar(&f.relayService, "relay-service", "", "run the local relay as a background service: on or off (default: on, or your last choice)")
	cmd.Flags().BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap "+app.statusLineOwner()+"'s status line (which captures the plan's rate-limit windows)")
	cmd.Flags().StringVar(&f.identity, "identity", "", "identity stamped on the sessions of agents that take one (default: git user.email; \"none\" to omit)")
	cmd.Flags().StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
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
	fmt.Fprintf(out, "%s in %s\n", ui.p.Bold("Installing terma"), output.TildePath(root))
	existing, err := install.Open(app.agents, root, gitDir)
	if err != nil {
		return err
	}

	// Agents are resolved first because they decide whether sign-in is needed.
	agents, chosen, err := app.resolveInstallHarnesses(cmd, cfg, f)
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	if err != nil {
		return err
	}
	// A flag value that is refused is refused before anything signs in or prints.
	switch f.relayService {
	case "", "on", "off":
	default:
		return fmt.Errorf("--relay-service %q: want on or off", f.relayService)
	}

	req := install.Request{Root: root, GitDir: gitDir, Existing: existing, ProjectRef: f.projectRef, Selected: agents,
		RecordSelected: chosen && !f.dryRun, Adapters: splitCommas(f.adapters), NoHooks: f.noHooks, DryRun: f.dryRun,
		AssumeYes: f.assumeYes, CanAsk: canPrompt(), Version: app.version, Now: time.Now()}
	// A picker left before anything is written; a confirm declined later is the plan's own.
	unbound := false
	flow := install.Workflow{
		SignIn: func(_ context.Context, cfg *config.Config) (*config.Config, error) {
			return app.signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser})
		},
		Bind: func(_ context.Context, cfg *config.Config, existing *termaproject.File, ref string, verify, ask bool) (install.Binding, error) {
			b, err := app.resolveBinding(cmd, cfg, existing, ref, verify, ask)
			unbound = errors.Is(err, errCancelled)
			if errors.Is(err, auth.ErrNotLoggedIn) {
				return b, fmt.Errorf("%w: %w", install.ErrNotSignedIn, err)
			}
			return b, err
		},
		FetchPolicy:    app.policies().Fetch,
		HasKey:         func(projectID string) bool { return keystore.Get(projectID) != "" },
		RefreshMachine: app.refresher().Machine,
		ApplySteps: func(cfg *config.Config, plan install.Plan) install.Steps {
			return app.installSteps(cmd, ui, cfg, agents, f, plan)
		},
	}
	plan, err := install.Run(ctx, app.agents, cfg, req, flow, ui)
	if unbound {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return nil
	}
	// What already succeeded is still shown when a later step fails or is declined.
	if err != nil || f.dryRun {
		ui.printLines()
		return err
	}
	// The hooks call terma by name: an install they cannot run is not done.
	if exe, err := os.Executable(); err == nil {
		if c := doctor.BinaryCheck(exe, app.binDirs(), doctor.ByName); c.Status == doctor.Fail {
			ui.Warn("terma", "not on your PATH, so the hooks cannot run it")
			ui.next = append([]string{"Put terma on your PATH: `" + doctor.AddToPathCommand(filepath.Dir(exe)) + "`"}, ui.next...)
		}
	}
	ui.title = "Installed — this repository reports to " + cmp.Or(plan.Binding.Name, plan.Binding.ID, "your team")
	ui.warnTitle = "Almost done — finish the steps marked ! below"
	ui.finish()
	return nil
}

// installSteps are what an install does outside the repository.
func (app *App) installSteps(cmd *cobra.Command, ui *installUI, cfg *config.Config, agents []string, f installFlags, plan install.Plan) install.Steps {
	steps := install.Steps{
		Confirm: func(question string, explain []string) (bool, error) {
			if !f.verbose {
				explain = nil
			}
			yes, err := confirmExplained(cmd, question, explain, true)
			if errors.Is(err, errCancelled) {
				return false, err
			}
			return err == nil && yes, nil
		},
		// The per-developer half: home-directory state, no committed file.
		Connect: func(context.Context) error { return app.connectHarnessesForRepo(cmd, ui, cfg, agents, f, plan) },
		SpoolKey: func(ctx context.Context) (string, string) {
			sp := spinner.New(cmd.ErrOrStderr())
			sp.Start("Preparing hook event delivery…")
			defer sp.Stop()
			k := app.ensureSpoolKey(ctx, cfg)
			return k.state, k.fix
		},
		RepoPolicy: func(_ context.Context, hs []harness.Harness) ([]string, error) {
			signals, err := harness.ParseSignals(f.signals)
			if err != nil {
				return nil, err
			}
			return install.WriteRepoPolicy(ui, plan.Root, hs, plan.Exporter(cfg.OTLPURL, signals), f.updatePolicy)
		},
	}
	// The status line is a global setting, wrapped only for a developer who chose its agent.
	if a, ok := doctor.StatusLineAgent(app.agents); ok && !f.noStatusLine && slices.Contains(agents, a.Name()) {
		steps.StatusLine = func() (string, bool) { return app.installStatusLine(cmd.ErrOrStderr()) }
	}
	return steps
}

// spoolKey's fix, when set, is what the developer must do before events are delivered.
type spoolKey struct{ state, fix string }

// ensureSpoolKey never fails the install: held events wait up to the spool's MaxAge for a key.
func (app *App) ensureSpoolKey(ctx context.Context, cfg *config.Config) spoolKey {
	if keystore.Get(cfg.ProjectID) != "" {
		return spoolKey{state: "delivered with this team's key"}
	}
	const held = "held until this machine has a key for the team"
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
	return spoolKey{state: "Team key stored for this machine (" + keystore.Mask(key) + ")"}
}

// resolveInstallHarnesses never reads the committed binding: agents are a per-developer
// choice. chosen is a choice made just now, which the install records once admitted.
func (app *App) resolveInstallHarnesses(cmd *cobra.Command, cfg *config.Config, f installFlags) (names []string, chosen bool, err error) {
	if h := strings.TrimSpace(f.harnesses); h != "" {
		if strings.EqualFold(h, "none") {
			return nil, false, nil
		}
		names, err := app.parseAgentList(f.harnesses)
		return names, false, err
	}
	if len(cfg.Harnesses) > 0 {
		recorded := map[string]bool{}
		for _, name := range cfg.Harnesses {
			recorded[name] = true
		}
		if names := app.selectedInRegistryOrder(recorded); len(names) > 0 {
			return names, false, nil
		}
	}
	names, err = app.chooseHarnesses(cmd, cfg, setupFlags{assumeYes: f.assumeYes})
	return names, err == nil, err
}

// connectHarnessesForRepo writes no committed file: keys, the routing record and the
// relay are home-directory state.
func (app *App) connectHarnessesForRepo(cmd *cobra.Command, ui *installUI, cfg *config.Config, agents []string, f installFlags, plan install.Plan) error {
	ctx := cmd.Context()
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	rec, ok := install.RouteRecord(app.agents, cfg.ProjectID, agents, plan.Exporter(cfg.OTLPURL, signals))
	if !ok {
		return nil
	}
	if !cmd.Flags().Changed("signals") && plan.Record != nil {
		rec.Signals = plan.Record.Signals
	}
	sp := spinner.New(cmd.ErrOrStderr())
	defer sp.Stop()
	for _, a := range rec.Harnesses {
		h, err := app.agents.Harness(a)
		if err != nil {
			continue // an exporter terma writes: it sends with the project's spool key
		}
		// Either key on file serves the relay, so none is minted.
		if keystore.GetFor(a, cfg.ProjectID) != "" || keystore.Get(cfg.ProjectID) != "" {
			continue
		}
		sp.Start("Preparing " + h.DisplayName() + "'s key for this team…")
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

func splitCommas(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
