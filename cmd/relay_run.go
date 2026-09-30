package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

func newRelayRunCommand() *cobra.Command {
	var idle time.Duration
	var addr string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the relay in the foreground until it has been idle for --idle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := relayDir()
			if err != nil {
				return err
			}
			// --idle 0 is the service's relay: it outlasts whichever relay a hook started
			// while it was down, taking over when that one exits, and a machine whose
			// relay setup is gone leaves it stopped (exit 0, see ExitRestart).
			service := idle <= 0
			if service {
				if _, err := relayToken(); err != nil {
					return nil
				}
			}
			unlock, busy, err := lockRelay(cmd.Context(), dir, service)
			if err != nil {
				return err
			}
			if unlock == nil {
				if busy && !quiet {
					fmt.Fprintln(cmd.OutOrStdout(), "The relay is already running.")
				}
				return nil
			}
			defer unlock()
			token, err := relayToken()
			if err != nil {
				return err
			}
			if addr == "" {
				addr = relayAddr(dir)
			}
			cfg, err := relayRunConfig()
			if err != nil {
				return err
			}
			opts := relayRunOptions(cmd, dir, token, cfg)
			r := relay.New(opts)
			ln, err := listenRelay(dir, addr)
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Relay listening on %s (hold %s, idle exit %s).\n", ln.Addr(), opts.Hold, idle)
			}
			claim.Prune(time.Now())
			pidPath := filepath.Join(dir, relayPIDFile)
			_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
			defer func() { _ = os.Remove(pidPath) }()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			// A client that trickles its request cannot hold a connection open.
			srv := &http.Server{Handler: r.Handler(), ConnContext: r.ConnContext, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
			go func() { _ = srv.Serve(ln) }()
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			go pollCollectionPolicy(ctx)
			replaced, setupGone := watchRelay(ctx, r, dir, idle)
			cancel()
			shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = srv.Shutdown(shutdown)
			<-done
			if data, err := json.MarshalIndent(r.Stats().Snapshot(), "", "  "); err == nil {
				_ = config.WriteFileAtomicNoSync(filepath.Join(dir, relayStatsFile), append(data, '\n'), 0o600)
			}
			// Under a service manager, the new binary starts now; a relay a hook started
			// is started again by the next hook. The service's relay asks to be started
			// again whenever it was stopped for any reason but its setup being gone:
			// `terma relay setup` and `terma setup` stop it so that it rereads the token,
			// address and policy. Removing the service (launchctl bootout, systemctl
			// disable, the Windows launcher removed) never restarts it, whatever it exits.
			if replaced || service && !setupGone {
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

// lockRelay takes the relay's single-instance lock. With wait — the service's relay —
// it waits out a relay a hook started. A nil unlock means this relay must not run:
// busy when another holds the lock, neither when ctx ended the wait.
func lockRelay(ctx context.Context, dir string, wait bool) (unlock func(), busy bool, err error) {
	path := filepath.Join(dir, relayLockFile)
	unlock, err = flock.TryLock(path)
	for wait && flock.IsBusy(err) {
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(5 * time.Second):
		}
		unlock, err = flock.TryLock(path)
	}
	if flock.IsBusy(err) {
		return nil, true, nil
	}
	return unlock, false, err
}

// relayRunConfig is the configuration a relay routes with. Before the first successful
// policy fetch, capture is disabled; the background poll authorizes it without
// delaying the loopback listener.
func relayRunConfig() (*config.Config, error) {
	cfg, err := loadConfig()
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
func relayRunOptions(cmd *cobra.Command, dir, token string, cfg *config.Config) relay.Options {
	hold := relay.DefaultHold
	if v, err := time.ParseDuration(os.Getenv("TERMA_RELAY_HOLD")); err == nil && v > 0 {
		hold = v
	}
	beat, _ := time.ParseDuration(os.Getenv("TERMA_RELAY_HEARTBEAT"))
	minter := newRelayKeyMinter(cmd.Context(), cfg)
	opts := relay.Options{Token: token, Hold: hold, Dir: filepath.Join(dir, relay.OutboxDir), Resolve: relayResolver(cfg, minter.mint), Version: Version,
		CatchAll: relayCatchAll(), HeartbeatInfo: relayHeartbeatInfo(dir), HeartbeatSend: relayHeartbeatSend, HeartbeatEvery: max(beat, 0),
		PeerPID: procinfo.FindSender, ProcessAlive: harness.ProcessAlive, ClaimCacheTTL: time.Second, PolicyCacheTTL: time.Second}
	if os.Getenv("TERMA_RELAY_DEBUG") == "1" {
		errOut := cmd.ErrOrStderr()
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(errOut, time.Now().Format("15:04:05.000 ")+f+"\n", a...) }
	}
	return opts
}

// listenRelay opens the relay's address. A hook started this relay with nowhere to
// print, so a failure is also written where status reads it (and spawnRelay backs off).
func listenRelay(dir, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		err = fmt.Errorf("relay: listen on %s: %w", addr, err)
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, relayErrorFile), []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
		return nil, err
	}
	_ = os.Remove(filepath.Join(dir, relayErrorFile))
	return ln, nil
}

// watchRelay checks the running relay every second until it should stop — ctx ended,
// its setup gone, a stop requested, its binary replaced, or idle for idle (never when
// idle is 0) — and says whether it was replaced or its setup is gone.
func watchRelay(ctx context.Context, r *relay.Relay, dir string, idle time.Duration) (replaced, setupGone bool) {
	self := executableStamp()
	lastPrune := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, false
		case <-tick.C:
		}
		d, quiet := r.Idle()
		// Setup undone — terma uninstalled, the config directory removed: the agents no
		// longer point here, so there is nothing to relay for.
		if _, err := relayToken(); err != nil {
			return false, true
		}
		if stopRequested(dir) {
			return false, false
		}
		if time.Since(lastPrune) > time.Hour {
			claim.Prune(time.Now())
			lastPrune = time.Now()
		}
		// A newer terma replaced this binary (update, reinstall): step aside once quiet,
		// and the next hook starts the new one. Not mid-export — agents do not retry a
		// refused connection.
		if quiet && d >= time.Minute && self != "" && executableStamp() != self {
			return true, false
		}
		if quiet && idle > 0 && d >= idle {
			return false, false
		}
	}
}

// stopRequested reports whether stopRelay asked this relay to stop through the stop
// file, and takes the request. One naming another pid is a dead relay's, and is cleared.
func stopRequested(dir string) bool {
	path := filepath.Join(dir, relayStopFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid == os.Getpid()
}

// executableStamp identifies the file this process was started from — its size and
// modification time — so a relay can tell it has been replaced. Empty when unknown.
func executableStamp() string {
	exe := stableExecutable()
	if exe == "" {
		return ""
	}
	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}
