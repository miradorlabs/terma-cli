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

	"github.com/miradorlabs/terma-cli/internal/auth"
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
			// Under a service manager, the new binary starts now; a relay a hook started is
			// started again by the next hook. `terma relay setup` and `terma setup` stop
			// the service's relay so that it rereads the token, address and policy.
			// Removing the service never restarts it, whatever it exits.
			if res.Restart() {
				return exitWith(ExitRestart)
			}
			return nil
		},
	}
	// Long, because a relay that is not running when an agent starts loses what the
	// agent exports before its first hook: Codex's conversation_starts comes before
	// SessionStart (docs/RELAY.md). A relay a hook started stays for the next one.
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma relay setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	return cmd
}

// relayRunConfig is the configuration a relay routes with. Before the first successful
// policy fetch, capture is disabled; the background poll authorizes it without
// delaying the loopback listener.
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

// relayRunOptions wires the relay to this machine: its token and outbox, the policy
// and catch-all resolvers, the heartbeat, and how senders are found. TERMA_RELAY_HOLD
// and TERMA_RELAY_HEARTBEAT shorten the hold and the heartbeat's period for a test;
// TERMA_RELAY_DEBUG=1 logs every drop.
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
