package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

func (app *App) newRelayRunCommand() *cobra.Command {
	var idle time.Duration
	var addr string
	var quiet, successor bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the relay in the foreground until it has been idle for --idle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir()
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
			engine := deps.Engine(ctx, dir, cfg, daemon.SettingsFromEnv(), cmd.ErrOrStderr())
			engine.Warnf = log.Printf
			res, err := daemon.Run(ctx, daemon.Config{
				Dir: dir, Addr: addr, Idle: idle, Environment: cfg.Environment, Log: log,
				Successor: successor, SpawnSuccessor: func() error { return daemon.SpawnSuccessor(dir) },
				Engine:  engine,
				Workers: []func(context.Context){deps.Refresher().Run},
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
			// Exit for the service manager to restart this as the new binary; a
			// hook-started relay is restarted by the next hook.
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
	// A stopping relay starts its successor with this, to take its socket once it has drained.
	cmd.Flags().BoolVar(&successor, "successor", false, "wait for the running relay to stop, and take over its socket")
	_ = cmd.Flags().MarkHidden("successor")
	return cmd
}
