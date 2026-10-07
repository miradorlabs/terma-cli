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
)

func (app *App) newRelayRunCommand() *cobra.Command {
	var idle time.Duration
	var addr string
	var quiet, asService bool
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
			res, err := daemon.Run(ctx, daemon.Config{
				StateDir: app.stateDir, Addr: addr, Idle: idle, Service: asService, Environment: cfg.Environment, Version: app.version, Log: log,
				Engine:  engine,
				Workers: []func(context.Context){deps.Refresher().Run, app.sweepHookState},
				Updater: app.relayUpdater(),
				Listening: func(at net.Addr, hold time.Duration) {
					if !quiet {
						fmt.Fprintf(cmd.OutOrStdout(), "Relay listening on %s (hold %s, idle exit %s).\n", at, hold, idle)
					}
				},
			})
			if err != nil {
				return err
			}
			if res.AlreadyRunning && !quiet {
				fmt.Fprintln(cmd.OutOrStdout(), "The relay is already running.")
			}
			// Exit for the service manager to start the newer terma, whoever installed it;
			// a hook-started relay is restarted by the next hook.
			if res.Restart() {
				return exitWith(ExitRestart)
			}
			return nil
		},
	}
	// Long, because an agent may export before its first hook.
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	// Only the service definitions pass it: setup replaces a relay that was started any other way.
	cmd.Flags().BoolVar(&asService, "service", false, "run as the relay service")
	_ = cmd.Flags().MarkHidden("service")
	return cmd
}

// relayUpdater has the relay install each new release in place of its own binary, as
// interactive commands do, so a machine nobody runs terma on still updates. It is nil where
// the relay could never install one: a development build, a package manager's or a Windows
// binary, or a process with updates turned off, as in CI. Turned off with `terma update
// --auto off`, it asks nothing, not even GitHub.
func (app *App) relayUpdater() *daemon.Updater {
	exe, err := os.Executable()
	if err != nil || !selfupdate.IsRelease(app.version) || !selfupdate.UpdatesItself(exe) ||
		os.Getenv("CI") != "" || os.Getenv("TERMA_NO_UPDATE_CHECK") == "1" {
		return nil
	}
	client := &selfupdate.Client{Version: app.version}
	return &daemon.Updater{Every: daemon.UpdateEvery, Update: func(ctx context.Context) (string, error) {
		if p, err := selfupdate.LoadPreferences(app.dir); err != nil || !p.Auto {
			return "", nil
		}
		o := client.Auto(ctx, app.dir, app.stateDir, exe, nil)
		return o.Installed, o.Err
	}}
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
