package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// The local relay spike (docs/RELAY-SPIKE.md): the agents' global exporters send to
// terma on loopback, and only what a hook in an opted-in repository claimed goes on
// to Terma. Every command here is hidden while it is a spike.

// defaultRelayAddr is where the relay listens unless `terma relay setup --addr` said
// otherwise. It is fixed, because the agents' exporter configuration is static.
const defaultRelayAddr = "127.0.0.1:43180"

const (
	relayAddrFile  = "addr"
	relayLockFile  = "relay.lock"
	relayStatsFile = "stats.json"
	relayPIDFile   = "pid"
)

func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "Spike: the local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(newRelayRunCommand(), newRelaySetupCommand(), newRelayStatusCommand())
	return cmd
}

func relayDir() (string, error) {
	dir, err := claim.Dir()
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// relayAddr is the address the relay listens on: what setup recorded, else the default.
func relayAddr(dir string) string {
	if data, err := os.ReadFile(filepath.Join(dir, relayAddrFile)); err == nil {
		if a := strings.TrimSpace(string(data)); a != "" {
			return a
		}
	}
	return defaultRelayAddr
}

func relayToken() (string, error) {
	path, err := claim.TokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("the relay is not set up on this machine — run `terma relay setup`")
	}
	return strings.TrimSpace(string(data)), nil
}

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
			unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
			if flock.IsBusy(err) {
				if !quiet {
					fmt.Fprintln(cmd.OutOrStdout(), "The relay is already running.")
				}
				return nil
			}
			if err != nil {
				return err
			}
			defer unlock()
			token, err := relayToken()
			if err != nil {
				return err
			}
			if addr == "" {
				addr = relayAddr(dir)
			}
			hold := relay.DefaultHold
			if v, err := time.ParseDuration(os.Getenv("TERMA_RELAY_HOLD")); err == nil && v > 0 {
				hold = v
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			r := relay.New(relay.Options{Token: token, Hold: hold, Resolve: relayResolver(cfg), Version: Version})
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("relay: listen on %s: %w", addr, err)
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Relay listening on %s (hold %s, idle exit %s).\n", ln.Addr(), hold, idle)
			}
			claim.Prune(time.Now())
			pidPath := filepath.Join(dir, relayPIDFile)
			_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
			defer func() { _ = os.Remove(pidPath) }()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = srv.Serve(ln) }()
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			for tick := time.NewTicker(time.Second); ; {
				select {
				case <-ctx.Done():
				case <-tick.C:
					if d, ok := r.Idle(); !ok || idle <= 0 || d < idle {
						continue
					}
					cancel()
				}
				tick.Stop()
				break
			}
			shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = srv.Shutdown(shutdown)
			cancel()
			<-done
			if data, err := json.MarshalIndent(r.Stats().Snapshot(), "", "  "); err == nil {
				_ = config.WriteFileAtomicNoSync(filepath.Join(dir, relayStatsFile), append(data, '\n'), 0o600)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&idle, "idle", 30*time.Minute, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma relay setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	return cmd
}

// relayResolver turns a claim into where its session's telemetry goes: the project's
// own ingest host (projectEndpoint, the spool's order), the key this machine holds for
// the claiming agent (else the project's spool key), and what the routing record lets
// through. No key means the developer never opted in to this project here; no routing
// record means no one chose, so content is withheld.
func relayResolver(cfg *config.Config) func(claim.Claim) (relay.Policy, error) {
	return func(c claim.Claim) (relay.Policy, error) {
		key := keystore.GetFor(harnessForTool(c.Tool), c.ProjectID)
		if key == "" {
			key = keystore.Get(c.ProjectID)
		}
		if key == "" {
			return relay.Policy{}, relay.ErrNoKey
		}
		pol := relay.Policy{Endpoint: projectEndpoint(cfg, c.ProjectID), Key: key}
		if rec, ok, err := shim.LoadRecord(c.ProjectID); err == nil && ok {
			pol.IncludePrompts, pol.IncludeToolContent = rec.IncludePrompts, rec.IncludeToolContent
		}
		return pol, nil
	}
}

// harnessForTool maps a hook's tool label to the keystore's harness name.
func harnessForTool(tool string) string {
	if tool == "claude-code" {
		return "claude"
	}
	return tool
}

func newRelaySetupCommand() *cobra.Command {
	var addr, agents string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Point the agents' global exporters at the local relay",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := relayDir()
			if err != nil {
				return err
			}
			token, err := ensureRelayToken()
			if err != nil {
				return err
			}
			if addr == "" {
				addr = relayAddr(dir)
			}
			if err := config.WriteFileAtomic(filepath.Join(dir, relayAddrFile), []byte(addr+"\n"), 0o600); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// The relay withholds content per project, so the exporters send it all.
			exp := harness.Exporter{Endpoint: "http://" + addr, APIKey: token, Signals: harness.AllSignals, IncludePrompts: true, IncludeToolContent: true}
			for name := range strings.SplitSeq(agents, ",") {
				h, err := harness.Lookup(strings.TrimSpace(name))
				if err != nil {
					return err
				}
				if err := h.Connect(exp, true); err != nil {
					return fmt.Errorf("%s: %w", h.DisplayName(), err)
				}
				fmt.Fprintf(out, "%s exports to the relay at %s.\n", h.DisplayName(), addr)
			}
			fmt.Fprintln(out, "Hooks in repositories with a binding start the relay and claim their sessions; nothing else is forwarded.")
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "the loopback address the relay listens on (default "+defaultRelayAddr+")")
	cmd.Flags().StringVar(&agents, "harness", "claude,codex", "the agents to point at the relay")
	return cmd
}

func ensureRelayToken() (string, error) {
	if token, err := relayToken(); err == nil && token != "" {
		return token, nil
	}
	path, err := claim.TokenPath()
	if err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	return token, config.WriteFileAtomic(path, []byte(token+"\n"), 0o600)
}

func newRelayStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the relay is running and what it has done",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := relayDir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			snap, running, err := relayStats(dir)
			if err != nil {
				return err
			}
			if running {
				fmt.Fprintf(out, "Running on %s since %s.\n", relayAddr(dir), snap.Since.Format(time.RFC3339))
			} else {
				fmt.Fprintln(out, "Not running. Last run:")
			}
			for _, k := range snap.Keys() {
				fmt.Fprintf(out, "  %-44s %d\n", k, snap.Counters[k])
			}
			return nil
		},
	}
}

// relayStats reads the running relay's stats, else those the last run left behind.
func relayStats(dir string) (relay.Snapshot, bool, error) {
	var snap relay.Snapshot
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err == nil {
		unlock()
		data, err := os.ReadFile(filepath.Join(dir, relayStatsFile))
		if err != nil {
			return snap, false, nil
		}
		return snap, false, json.Unmarshal(data, &snap)
	}
	token, err := relayToken()
	if err != nil {
		return snap, true, err
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+relayAddr(dir)+"/stats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return snap, true, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return snap, true, json.Unmarshal(body, &snap)
}

// spawnRelay starts `terma relay run` detached, unless one is already running. A hook
// calls it after claiming a session, so the relay is up before the session's first
// export in the common case and restarted if it idled out; two hooks racing here both
// start one and the second exits at its lock.
func spawnRelay() {
	dir, err := claim.Dir()
	if err != nil {
		return
	}
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err != nil {
		return // running (or unlockable: nothing to do from a hook either way)
	}
	unlock()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	proc := exec.Command(exe, "relay", "run", "--quiet")
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
}
