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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// The local relay spike (docs/RELAY-SPIKE.md): the agents' global exporters send to
// terma on loopback, and only what a hook in an opted-in repository claimed goes on
// to Terma. Every command here is hidden while it is a spike.

// defaultRelayAddr is where the relay listens by default (claim.DefaultAddr).
const defaultRelayAddr = claim.DefaultAddr

const (
	relayAddrFile  = "addr"
	relayLockFile  = "relay.lock"
	relayStatsFile = "stats.json"
	relayPIDFile   = "pid"
	relayErrorFile = "last-error"
)

// relayRetryAfter is how long hooks leave a relay that failed to start before trying
// again.
const relayRetryAfter = time.Minute

func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "Spike: the local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(newRelayRunCommand(), newRelaySetupCommand(), newRelayStatusCommand(), newRelayDaemonCommand())
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
			opts := relay.Options{Token: token, Hold: hold, Resolve: relayResolver(cfg), Version: Version,
				PeerPID: procinfo.FindSender, ClaimCacheTTL: time.Second, PolicyCacheTTL: 5 * time.Second}
			if os.Getenv("TERMA_RELAY_DEBUG") == "1" {
				errOut := cmd.ErrOrStderr()
				opts.Logf = func(f string, a ...any) { fmt.Fprintf(errOut, time.Now().Format("15:04:05.000 ")+f+"\n", a...) }
			}
			r := relay.New(opts)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				// A hook started this relay with nowhere to print; status reads it.
				err = fmt.Errorf("relay: listen on %s: %w", addr, err)
				_ = config.WriteFileAtomicNoSync(filepath.Join(dir, relayErrorFile), []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
				return err
			}
			_ = os.Remove(filepath.Join(dir, relayErrorFile))
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
			// A client that trickles its request cannot hold a connection open.
			srv := &http.Server{Handler: r.Handler(), ConnContext: r.ConnContext, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
			go func() { _ = srv.Serve(ln) }()
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			self := executableStamp()
			lastPrune := time.Now()
			for tick := time.NewTicker(time.Second); ; {
				select {
				case <-ctx.Done():
				case <-tick.C:
					d, quiet := r.Idle()
					// Setup undone — terma uninstalled, the config directory removed: the
					// agents no longer point here, so there is nothing to relay for.
					if _, err := relayToken(); err != nil {
						cancel()
						break
					}
					if time.Since(lastPrune) > time.Hour {
						claim.Prune(time.Now())
						lastPrune = time.Now()
					}
					// A newer terma replaced this binary (update, reinstall): step aside
					// once quiet, and the next hook starts the new one. Not mid-export —
					// agents do not retry a refused connection.
					if quiet && d >= time.Minute && self != "" && executableStamp() != self {
						cancel()
						break
					}
					if !quiet || idle <= 0 || d < idle {
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
	// Long, because a relay that is not running when an agent starts loses what the
	// agent exports before its first hook: Codex's conversation_starts comes before
	// SessionStart (docs/RELAY-SPIKE.md). A relay a hook started stays for the next one.
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma relay setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	return cmd
}

// putOmpOnRelay installs omp's PATH shim and puts the shim directory on PATH through
// the startup-file block `terma install` maintains.
func putOmpOnRelay(out io.Writer) error {
	binDir, err := shim.InstallShims([]string{shim.AgentOmp})
	if err != nil {
		return err
	}
	if shim.Active(shim.AgentOmp) {
		return nil
	}
	rc, ok := shim.ShellRC()
	if !ok {
		fmt.Fprintf(out, "Add %s to the front of PATH so omp starts through terma's launcher.\n", binDir)
		return nil
	}
	if _, err := rc.Ensure(binDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "omp starts through terma's launcher (%s); run `%s` or open a new terminal.\n", tildePath(rc.Path), reloadCommand(tildePath(rc.Path)))
	return nil
}

// executableStamp identifies the file this process was started from — its size and
// modification time — so a relay can tell it has been replaced. Empty when unknown.
func executableStamp() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}

// stopRelay asks a running relay to stop (SIGTERM through its pid file) and waits for
// its lock, so a setup that changed the address or token takes effect.
func stopRelay(dir string) {
	data, err := os.ReadFile(filepath.Join(dir, relayPIDFile))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.SIGTERM) != nil {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile)); err == nil {
			unlock()
			return
		}
	}
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
	var noStart bool
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
				// omp reads its exporter's endpoint from the environment before any
				// extension runs, so the launcher hands it over: a PATH shim, in a bound
				// repository only (shim.ompRouter).
				if h.Name() == shim.AgentOmp {
					if err := putOmpOnRelay(out); err != nil {
						return err
					}
				}
			}
			fmt.Fprintln(out, "Hooks in repositories with a binding claim their sessions; nothing else is forwarded.")
			// A running relay has the old address and token: replace it. Then start one
			// now, so the first session does not open against a closed port.
			stopRelay(dir)
			if _, ok := relayServiceInstalled(); ok {
				// The service manager starts it again, with the new address and token.
			} else if !noStart {
				spawnRelay()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "the loopback address the relay listens on (default "+defaultRelayAddr+")")
	cmd.Flags().StringVar(&agents, "harness", "claude,codex", "the agents to point at the relay")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not start the relay now (the next hook that claims a session will)")
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
			if path, ok := relayServiceInstalled(); ok {
				fmt.Fprintf(out, "Service:  installed (%s)\n", path)
			}
			if running {
				fmt.Fprintf(out, "Running on %s since %s.\n", relayAddr(dir), snap.Since.Format(time.RFC3339))
			} else {
				if squatted(relayAddr(dir)) {
					fmt.Fprintf(out, "Warning: another process is listening on %s. The agents' telemetry goes to it, not to terma — stop it, or move the relay with `terma relay setup --addr`.\n", relayAddr(dir))
				}
				if data, err := os.ReadFile(filepath.Join(dir, relayErrorFile)); err == nil {
					fmt.Fprintf(out, "The relay last failed to start: %s", data)
				}
				fmt.Fprintln(out, "Not running. Last run:")
			}
			for _, k := range snap.Keys() {
				fmt.Fprintf(out, "  %-44s %d\n", k, snap.Counters[k])
			}
			return nil
		},
	}
}

// squatted reports whether something answers on the relay's address while the relay
// is not running: the agents' exporters would be sending to it.
func squatted(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
	// One that just failed to start (its port taken) is not retried by every hook:
	// each would start a process that fails the same way.
	if info, err := os.Stat(filepath.Join(dir, relayErrorFile)); err == nil && time.Since(info.ModTime()) < relayRetryAfter {
		return
	}
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
	// Wait, briefly, until it listens. The hook runs before its turn does, so an agent
	// that exports as soon as the hook returns — Claude Code 2.1.280 did, right after
	// UserPromptSubmit — finds the relay up instead of a refused connection it will not
	// retry. Only a hook that had to start the relay waits; the others return above.
	addr := relayAddr(dir)
	for deadline := time.Now().Add(relayStartWait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
	}
}

// relayStartWait bounds how long a hook that started the relay waits for it to listen.
const relayStartWait = time.Second

// relayDoctorCheck is doctor's "agent exporting to Terma" on a machine that exports
// through the local relay: the relay can run (or runs) on its address with no one else
// there, each of the developer's agents sends to it, and this repository's sessions
// can leave — it is bound and this machine holds its project's key.
func relayDoctorCheck(projectID string, agents []string) doctor.Check {
	dir, err := claim.Dir()
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	addr := relayAddr(dir)
	running := false
	if unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile)); err == nil {
		unlock()
	} else if flock.IsBusy(err) {
		running = true
	}
	if !running && squatted(addr) {
		return doctor.Check{Status: doctor.Fail, Detail: "another process is listening on " + addr + " and receives the agents' telemetry",
			Fix: "stop it, or move the relay with `terma relay setup --addr`"}
	}
	var wrong []string
	for _, name := range []string{shim.AgentClaude, shim.AgentCodex, "opencode"} {
		// OpenCode only for a developer who named it: its plugin is not set up by
		// default, and most machines have no OpenCode.
		if (len(agents) > 0 || name == "opencode") && !slices.Contains(agents, name) {
			continue
		}
		h, err := harness.Lookup(name)
		if err != nil {
			continue
		}
		if st, err := h.Status(); err != nil || !st.Connected || strings.TrimRight(st.Endpoint, "/") != "http://"+addr {
			wrong = append(wrong, h.DisplayName())
		}
	}
	if len(wrong) > 0 {
		return doctor.Check{Status: doctor.Fail, Detail: strings.Join(wrong, " and ") + " not exporting to the local relay", Fix: "terma relay setup"}
	}
	state := "starts with the next hook"
	if running {
		state = "running"
	}
	switch {
	case projectID == "":
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); this repository is not bound, so its sessions are never forwarded", Fix: "terma install"}
	case keystore.Get(projectID) == "" && keystore.GetFor("claude", projectID) == "" && keystore.GetFor("codex", projectID) == "":
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this project on this machine, so its sessions are dropped", Fix: "terma install"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: "through the local relay on " + addr + " (" + state + "); only this repository's sessions are forwarded"}
}
