package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

func (app *App) newRelayRunCommand() *cobra.Command {
	var idle time.Duration
	var addr string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the relay in the foreground until it has been idle for --idle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir()
			if err != nil {
				return err
			}
			cfg, err := app.relayRunConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := daemon.Run(ctx, daemon.Config{
				Dir: dir, Addr: addr, Idle: idle,
				Engine:  app.relayRunOptions(ctx, cmd, dir, cfg),
				Workers: []func(context.Context){app.policyRefresher().Run},
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
	// Long, because an agent may export before its first hook (docs/RELAY.md).
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma relay setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	return cmd
}

// relayRunConfig is the configuration a relay routes with; capture stays off until the
// background poll's first successful policy fetch.
func (app *App) relayRunConfig() (*config.Config, error) {
	cfg, err := app.loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.OrganizationID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
		if cred, err := auth.LoadCredential(cfg.ProfileName); err == nil && cred.CheckEnvironment(cfg.AuthURL) == nil {
			cfg.OrganizationID = cred.OrganizationID
		}
	}
	if cfg.Policy.FetchedAt.IsZero() || cfg.Policy.TeamID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
		cfg.Policy = config.Policy{Mode: config.ModeRepo, Signals: []string{}, OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL}
	}
	return cfg, nil
}

// relayRunOptions wires the relay to this machine; TERMA_RELAY_HOLD and
// TERMA_RELAY_HEARTBEAT shorten its timings for tests, TERMA_RELAY_DEBUG=1 logs drops.
func (app *App) relayRunOptions(ctx context.Context, cmd *cobra.Command, dir string, cfg *config.Config) relay.Options {
	hold := relay.DefaultHold
	if v, err := time.ParseDuration(os.Getenv("TERMA_RELAY_HOLD")); err == nil && v > 0 {
		hold = v
	}
	beat, _ := time.ParseDuration(os.Getenv("TERMA_RELAY_HEARTBEAT"))
	minter := app.newRelayKeyMinter(ctx, cfg)
	opts := relay.Options{Correlators: app.agents.With[shape.Correlator](), Capturers: app.agents.With[shape.Capturer](),
		Hold: hold, Dir: filepath.Join(dir, relay.OutboxDir), Resolve: app.relayResolver(cfg, minter.Mint), Version: app.version,
		CatchAll: relayCatchAll(), HeartbeatInfo: app.relayHeartbeat(dir).Info(), HeartbeatSend: app.relayHeartbeatSend, HeartbeatEvery: max(beat, 0),
		PeerPID: procinfo.FindSender, ProcessAlive: harness.ProcessAlive, ClaimCacheTTL: time.Second, PolicyCacheTTL: time.Second}
	if os.Getenv("TERMA_RELAY_DEBUG") == "1" {
		errOut := cmd.ErrOrStderr()
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(errOut, time.Now().Format("15:04:05.000 ")+f+"\n", a...) }
	}
	return opts
}
