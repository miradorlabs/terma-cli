package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/miradorlabs/terma-cli/internal/routing"
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
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/exporter"
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
	// relayStopFile asks the relay whose pid it holds to stop: how stopRelay reaches a
	// relay on Windows, which has no SIGTERM. The relay looks every second.
	relayStopFile = "stop"
)

// codexDaemonPredates is shared by relay diagnostics and heartbeat reporting.
var codexDaemonPredates = exporter.CodexDaemonPredates

const codexDaemonRestart = exporter.CodexDaemonRestart

// relayRetryAfter is how long hooks leave a failed start before trying again.
const relayRetryAfter = time.Minute

func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "Spike: the local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(newRelayRunCommand(), newRelaySetupCommand(), newRelayStatusCommand(), newRelayDaemonCommand(), newRelaySuperviseCommand())
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
			// --idle 0 is the service's relay: it outlasts whichever relay a hook started
			// while it was down, taking over when that one exits, and a machine whose
			// relay setup is gone leaves it stopped (exit 0, see ExitRestart).
			service := idle <= 0
			if service {
				if _, err := relayToken(); err != nil {
					return nil
				}
			}
			unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
			for service && flock.IsBusy(err) {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(5 * time.Second):
				}
				unlock, err = flock.TryLock(filepath.Join(dir, relayLockFile))
			}
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
			if cfg.OrganizationID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
				if cred, err := auth.LoadCredential(cfg.ProfileName); err == nil && cred.CheckEnvironment(cfg.AuthURL) == nil {
					cfg.OrganizationID = cred.OrganizationID
				}
			}
			// Before the first successful policy fetch, capture is disabled. The
			// background poll will authorize it without delaying the loopback listener.
			if cfg.Policy.FetchedAt.IsZero() || cfg.Policy.TeamID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
				cfg.Policy = config.Policy{Mode: config.ModeRepo, Signals: []string{}, OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL}
			}
			// TERMA_RELAY_HEARTBEAT shortens the heartbeat's period for a test.
			beat, _ := time.ParseDuration(os.Getenv("TERMA_RELAY_HEARTBEAT"))
			minter := newRelayKeyMinter(cmd.Context(), cfg)
			opts := relay.Options{Token: token, Hold: hold, Dir: filepath.Join(dir, relay.OutboxDir), Resolve: relayResolver(cfg, minter.mint), Version: Version,
				CatchAll: relayCatchAll(), HeartbeatInfo: relayHeartbeatInfo(dir), HeartbeatSend: relayHeartbeatSend, HeartbeatEvery: max(beat, 0),
				PeerPID: procinfo.FindSender, ProcessAlive: harness.ProcessAlive, ClaimCacheTTL: time.Second, PolicyCacheTTL: time.Second}
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
			go pollCollectionPolicy(ctx)
			self := executableStamp()
			replaced, setupGone := false, false
			lastPrune := time.Now()
			for tick := time.NewTicker(time.Second); ; {
				select {
				case <-ctx.Done():
				case <-tick.C:
					d, quiet := r.Idle()
					// Setup undone — terma uninstalled, the config directory removed: the
					// agents no longer point here, so there is nothing to relay for.
					if _, err := relayToken(); err != nil {
						setupGone = true
						cancel()
						break
					}
					if stopRequested(dir) {
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
						replaced = true
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
	// SessionStart (docs/RELAY-SPIKE.md). A relay a hook started stays for the next one.
	cmd.Flags().DurationVar(&idle, "idle", 8*time.Hour, "exit after this long with no export and nothing held or queued (0: never)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen here instead of the address `terma relay setup` recorded")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing")
	return cmd
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
	if err != nil {
		return
	}
	if proc.Signal(syscall.SIGTERM) != nil {
		// Windows signals nothing but a kill, which would skip the relay's delivery of
		// what it accepted: ask through the stop file instead.
		if config.WriteFileAtomicNoSync(filepath.Join(dir, relayStopFile), []byte(strconv.Itoa(pid)+"\n"), 0o600) != nil {
			return
		}
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
// through.
//
// No key yet — a repository the platform connected, where no `terma install` ran — asks
// mint for one in the background (relayKeyMinter): the session's parts wait in the hold
// meanwhile, as they do for any keyless claim. Content: the organization's policy
// (fetched by `terma setup`) is the ceiling, and the developer's routing record for the
// project can only narrow it; a record that exists and cannot be read withholds.
func relayResolver(cfg *config.Config, mint func(projectID string)) func(claim.Claim) (relay.Policy, error) {
	return func(c claim.Claim) (relay.Policy, error) {
		key := keystore.GetFor(harnessForTool(c.Tool), c.ProjectID)
		if key == "" {
			key = keystore.Get(c.ProjectID)
		}
		if key == "" {
			if mint != nil {
				mint(c.ProjectID)
			}
			return relay.Policy{}, relay.ErrNoKey
		}
		org := cfg.Policy
		// Reread the profile so a refreshed policy also governs queued exports. The
		// resolver's cache bounds these local reads; hooks never fetch the network.
		if file, err := config.LoadFile(); err != nil {
			return relay.Policy{}, err
		} else if p := file.Profiles[cfg.ProfileName]; p != nil {
			if p.OrganizationID != cfg.OrganizationID {
				return relay.Policy{}, errors.New("organization changed; restart the relay")
			}
			if p.Policy == nil || !p.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) || p.Policy.TeamID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
				org = config.Policy{Mode: config.ModeRepo, Signals: []string{}, OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL}
			} else {
				org = *p.Policy
			}
		}
		globalPrimary := org.Global() && (org.TeamID == "" || org.TeamID == c.ProjectID)
		org = routing.EffectivePolicy(org, c.ProjectID)
		if cfg.ProfileName != "" && org.FetchedAt.IsZero() && os.Getenv("TERMA_POLICY_STUB") == "" {
			// A new team's first exports wait for its background fetch, just as they
			// wait for a missing key. Unknown policy must neither grant nor drop them.
			return relay.Policy{}, errors.New("no validated collection policy for this team")
		}
		pol := relay.Policy{Endpoint: projectEndpoint(cfg, c.ProjectID), Key: key,
			IncludePrompts: org.IncludePrompts, IncludeToolContent: org.IncludeToolContent, Signals: org.Signals, ExcludePaths: org.ExcludePaths, RequireClaim: !globalPrimary || !org.Global()}
		// Native exporters do not identify the source files of arbitrary prompt,
		// response and tool blobs. With exclusions active, such text cannot be proven
		// safe; forward metadata and filter named excluded paths instead.
		if len(org.ExcludePaths) > 0 {
			pol.IncludePrompts, pol.IncludeToolContent = false, false
		}
		switch rec, ok, err := routing.LoadRecord(c.ProjectID); {
		case err != nil:
			pol.IncludePrompts, pol.IncludeToolContent = false, false
			pol.Signals = []string{}
		case ok:
			// Catch-all delivery has no tool and follows global policy. A claimed
			// session must also be one of this developer's selected harnesses;
			// another repository may already have configured its exporter globally.
			if c.Tool != "" && !slices.Contains(rec.Harnesses, exporter.NameForTool(c.Tool)) {
				pol.Signals = []string{}
				pol.IncludePrompts, pol.IncludeToolContent = false, false
				return pol, nil
			}
			pol.IncludePrompts = pol.IncludePrompts && rec.IncludePrompts
			pol.IncludeToolContent = pol.IncludeToolContent && rec.IncludeToolContent
			pol.Signals = []string{}
			for _, s := range rec.Signals {
				if org.AllowsSignal(s) {
					pol.Signals = append(pol.Signals, s)
				}
			}
		}
		return pol, nil
	}
}

// relayCatchAll is where global mode files what nothing placed: the organization's
// default project. The policy is read again at most every 10 seconds, so a `terma
// setup` that switches mode takes effect in a running relay (the service outlives it).
func relayCatchAll() func() (claim.Claim, bool) {
	var mu sync.Mutex
	var at time.Time
	var cached config.Policy
	return func() (claim.Claim, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > 10*time.Second {
			cached, at = hookPolicy(), time.Now()
		}
		if !cached.Global() || cached.DefaultProjectID == "" {
			return claim.Claim{}, false
		}
		return claim.Claim{ProjectID: cached.DefaultProjectID}, true
	}
}

// harnessForTool maps a hook's tool label to the keystore's harness name.
func harnessForTool(tool string) string {
	return exporter.NameForTool(tool)
}

// termaHookCommand is how an extension terma writes into an agent (Pi's, Hermes's)
// reaches `terma hook`: this terma by the path it was started as — ~/.local/bin/terma
// on an installed machine — so an agent started with another PATH (a desktop app gets
// the system's) still finds it. A path that cannot be had falls back to PATH.
func termaHookCommand() []string {
	if exe, err := relayServiceExecutable(); err == nil {
		return []string{exe, "hook"}
	}
	return []string{"terma", "hook"}
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
			err = pointAgentsAtRelay(cmd.Context(), splitCommas(agents), addr, token, func(agent, detail string) {
				fmt.Fprintf(out, "%s exports to the relay at %s%s.\n", agent, addr, detail)
			}, func(note string) { fmt.Fprintln(out, note) })
			if err != nil {
				return err
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

// pointAgentsAtRelay invokes exporter capabilities; configuration and reload
// requirements belong to the integrations in internal/relay/exporter.
func pointAgentsAtRelay(ctx context.Context, agents []string, addr, token string, done func(agent, detail string), note func(string)) error {
	dir, err := relayDir()
	if err != nil {
		return err
	}
	cfg := exporter.Config{Endpoint: "http://" + addr, Token: token, HookCommand: termaHookCommand(), StateDir: dir}
	for _, name := range agents {
		integration, err := exporter.Lookup(name)
		if err != nil {
			return err
		}
		result, err := integration.Configure(ctx, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", integration.DisplayName(), err)
		}
		if !result.Pending {
			paths := make([]string, 0, len(result.Paths))
			for _, path := range result.Paths {
				paths = append(paths, tildePath(path))
			}
			detail := ""
			if len(paths) > 0 {
				detail = " (" + strings.Join(paths, ", ") + ")"
			}
			done(integration.DisplayName(), detail)
		}
		for _, text := range result.Notes {
			note(text)
		}
	}
	return nil
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
			// What waits on disk for delivery: accepted for a claimed session, not yet
			// taken by its project's host. The next relay sends it.
			for _, q := range relay.Backlog(filepath.Join(dir, relay.OutboxDir)) {
				who := q.Project
				if q.Tool != "" {
					who += " (" + q.Tool + ")"
				}
				fmt.Fprintf(out, "Queued for %s: %d records in %d parts.\n", who, q.Records, q.Parts)
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
	if resp.StatusCode != http.StatusOK {
		return snap, true, fmt.Errorf("relay stats: HTTP %s", resp.Status)
	}
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
	// A package's tests run as <package>.test: that binary is no relay, and would only
	// fail on the flags while the caller waited for it to listen.
	if err != nil || strings.HasSuffix(filepath.Base(exe), ".test") {
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
	for _, name := range []string{routing.AgentClaude, routing.AgentCodex, "opencode"} {
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
	if slices.Contains(agents, routing.AgentCodex) || len(agents) == 0 {
		if d, ok := codexDaemonPredates(dir); ok {
			return doctor.Check{Status: doctor.Warn, Detail: fmt.Sprintf("Codex's background server (pid %d) started before Codex was pointed at the relay, and its threads still export where they did", d.PID),
				Fix: codexDaemonRestart}
		}
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
