package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func (app *App) newRelayRunCommand() *cobra.Command {
	var idle time.Duration
	var addr string
	var quiet, asService, follow bool
	var launch daemon.Launch
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the relay in the foreground until it has been idle for --idle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir(app.stateDir)
			if err != nil {
				return err
			}
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}
			daemon.Prepare(cfg)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			deps := app.relayDeps()
			log := daemon.NewLog(dir)
			defer log.Close()
			engine := deps.Engine(ctx, app.stateDir, cfg, daemon.SettingsFromEnv(), cmd.ErrOrStderr())
			engine.Warnf = log.Printf
			// Read once: an upgrade restarts the relay.
			install := app.installed()
			engine.InstallMethod = install.Method
			engine.UpdateAuto = func() string { return app.updateAuto(install) }
			res, err := daemon.Run(ctx, daemon.Config{
				StateDir: app.stateDir, Addr: addr, Idle: idle, Service: asService, Follow: follow, Launch: launch, Environment: cfg.Environment, Version: app.version, Log: log,
				Engine:  engine,
				Workers: []func(context.Context){deps.Refresher().Run, app.sweepHookState},
				Updater: app.relayUpdater(install),
				Listening: func(at net.Addr, hold time.Duration) {
					if !quiet {
						fmt.Fprintf(cmd.OutOrStdout(), "Relay listening on %s (hold %s, idle exit %s).\n", at, hold, idle)
					}
				},
			})
			if err != nil {
				if res.LastBeatDue() {
					res.ExitBeat()
				}
				return err
			}
			if res.AlreadyRunning && !quiet {
				fmt.Fprintln(cmd.OutOrStdout(), "The relay is already running.")
			}
			return app.afterRelay(res)
		},
	}
	// Long, because an agent may export before its first hook.
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	// Only the service definitions pass it: setup replaces a relay that was started any other way.
	cmd.Flags().BoolVar(&asService, "service", false, "run as the relay service")
	// Only Spawn passes it, for a hook or a command such as setup, so the heartbeat tells a
	// relay terma started on demand from one a developer did; --service says the service's.
	cmd.Flags().Var(&launch, "launch", "what started this relay: hook or manual")
	_ = cmd.Flags().MarkHidden("launch")
	// Only Supersede passes it, starting the relay that takes over from the one it asked to
	// make way.
	cmd.Flags().BoolVar(&follow, "follow", false, "wait for the running relay to stop, then run in its place")
	_ = cmd.Flags().MarkHidden("follow")
	_ = cmd.Flags().MarkHidden("service")
	return cmd
}

// afterRelay ends a relay's run. A relay that should run again exits for the service manager
// to start the newer terma, whoever installed it. A hook-started relay has no manager: one
// that updated itself starts its successor now that its lock is free, since an agent may
// export before the next hook (unless teardown removed its setup meanwhile), and one replaced
// by a newer terma is followed by the relay Supersede started. The last heartbeat goes once
// that successor is under way (Result.LastBeatDue).
func (app *App) afterRelay(res daemon.Result) error {
	if res.Updated && !res.Service && !res.SetupGone {
		app.spawnRelay(app.stateDir, app.version)
	}
	if res.LastBeatDue() {
		res.ExitBeat()
	}
	if res.Restart() {
		return exitWith(ExitRestart)
	}
	return nil
}

// relayUpdater has the relay install each new release in place of its own binary, as
// interactive commands do, so a machine nobody runs terma on still updates. It is nil where
// the relay could never install one (updateAuto says unsupported): a development build, a
// package manager's or a Windows binary, or one this process could not find. Each pass asks
// updateAuto first and installs only when it says on, so the relay does what its heartbeat
// reports: turned off with `terma update --auto off`, or in CI, it asks nothing, not even
// GitHub.
func (app *App) relayUpdater(install selfupdate.Install) *daemon.Updater {
	if app.updateAuto(install) == semconv.TermaUpdateAutoUnsupported {
		return nil
	}
	// One client for the relay's life: it goes on as each release it installs, so the next
	// is installed over that one (selfupdate.Client.Auto).
	client := app.updateClient()
	return &daemon.Updater{Every: daemon.UpdateEvery, Update: func(ctx context.Context) (string, error) {
		if app.updateAuto(install) != semconv.TermaUpdateAutoOn {
			return "", nil
		}
		o := client.Auto(ctx, app.dir, app.stateDir, install.Exe, nil)
		return o.Installed, o.Err
	}}
}

// installed is how this terma was installed, as its relay reports it and updates it. A
// binary this process could not find, or could not tell as it started (App.binary), is
// one it never updates; one it found is still reported by its path.
func (app *App) installed() selfupdate.Install {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	install := selfupdate.InstallOf(exe, app.version)
	if app.binary == nil {
		install.Exe = ""
	}
	return install
}

// updateAuto is terma.update.auto for install as the developer's choice is now, read
// from config.json each time, since it can change while the relay runs. A choice terma
// cannot read is no choice to update: Client.Auto installs nothing then either.
func (app *App) updateAuto(install selfupdate.Install) string {
	p, _ := selfupdate.LoadPreferences(app.dir)
	return install.Automatic(p, os.Getenv)
}

// sweepHookState ages out the hooks' state now and hourly while the relay runs, so state
// no hook revisits still goes.
func (app *App) sweepHookState(ctx context.Context) {
	dirs := app.hookStateDirs()
	for {
		hookrun.Sweep(app.stateDir, time.Now(), dirs)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// hookStateDirs are the directories, under the state directory, that hooks keep state in.
func (app *App) hookStateDirs() []string {
	dirs := slices.Clone(hookrun.StateDirs)
	for _, k := range app.agents.With[agents.StateKeeper]() {
		dirs = append(dirs, k.StateDirs()...)
	}
	return dirs
}
