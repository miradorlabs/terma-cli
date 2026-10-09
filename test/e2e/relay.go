package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The local relay. UseRelay points the sandbox's agents
// at a relay on loopback instead of at the receiver; the receiver then stands in for
// Terma upstream, and what reaches it is what the relay chose to forward.

// RelayOptions shape a relay scenario.
type RelayOptions struct {
	// Start runs the relay before the agent. Without it the first hook that claims a
	// session starts it, as on a developer's machine — the cold start under test.
	Start bool
	// Service runs the relay before the agent the way this service manager does
	// (StartServiceRelay), started again after it fails.
	Service ServiceManager
	// Hold overrides how long unclaimed records wait (TERMA_RELAY_HOLD).
	Hold time.Duration
	// NoKey leaves the machine without a key for the project: set up, but never
	// opted in here.
	NoKey bool
	// Content is the team's collection policy, which the account fixture serves:
	// prompts and tool content through, or both withheld. Only the policy decides
	// content.
	Content bool
}

// UseRelay sets the relay up in the sandbox. It must run before the agent does.
func (sb *Sandbox) UseRelay(o RelayOptions) {
	t := sb.T
	t.Helper()
	acct := sb.StartAccount()
	acct.denyMints.Store(o.NoKey)
	if acct.withholdContent.Swap(!o.Content) != !o.Content {
		// The team's policy changed since setup fetched it: set up again, as a developer would.
		sb.Setup(nil, "--team", sb.ProjectID, "--harness", setupHarnesses)
		sb.Receiver.Reset()
	}
	sb.relayed = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sb.relayAddr = ln.Addr().String()
	_ = ln.Close()
	if o.Hold > 0 {
		sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_RELAY_HOLD="+o.Hold.String())
	}
	// Setup obtained a hook-delivery key from the account fixture. A no-key
	// negative control must remove it as well as refuse any subsequent key mint.
	projectKeys := map[string]string{}
	if !o.NoKey {
		projectKeys[sb.ProjectID] = liveKey
	}
	keys, _ := json.Marshal(map[string]any{"keys": projectKeys})
	if err := os.WriteFile(filepath.Join(sb.TermaConfig, "keys.json"), keys, 0o600); err != nil {
		t.Fatal(err)
	}
	// The relay withholds a claimed session of an agent the developer did not choose, and
	// setup records only Claude Code and Codex (the rest are Coming Soon): the profile
	// names every agent pointed at the relay, as setup would once it offers them.
	agents := append([]string{"claude", "codex", "opencode"}, sb.RelayAgents...)
	sb.recordHarnesses(agents)
	// --no-start: the scenario decides whether the relay runs before the agent.
	// One setup for every agent: a second would stop the relay StartRelay runs.
	sb.terma(sb.Repo, "relay", "setup", "--no-start", "--addr", sb.relayAddr, "--harness", strings.Join(agents, ","))
	t.Cleanup(sb.StopRelay)
	if o.Start {
		sb.StartRelay()
	}
	if o.Service != nil {
		sb.StartServiceRelay(o.Service)
	}
}

// ServiceManager says how long after a relay that ran for ran failed the service manager
// starts it again, as the service definitions ask (internal/relay/service).
type ServiceManager func(ran time.Duration) time.Duration

// Systemd waits RestartSec=5 after every failure.
func Systemd(time.Duration) time.Duration { return 5 * time.Second }

// Launchd starts a job no sooner than ThrottleInterval (5) after its previous start: a
// relay that ran longer is started again at once.
func Launchd(ran time.Duration) time.Duration { return max(0, 5*time.Second-ran) }

// StartServiceRelay runs `terma relay run --service` as manager does, started again after
// any exit but a clean one (systemd's Restart=on-failure, launchd's KeepAlive without
// SuccessfulExit), until the test ends.
func (sb *Sandbox) StartServiceRelay(manager ServiceManager) {
	t := sb.T
	t.Helper()
	sb.serviceRelay = true
	logPath := filepath.Join(sb.Dir, "relay.log")
	stop := make(chan struct{})
	done := make(chan struct{})
	// StopRelay stops the manager first, as `launchctl bootout` / `systemctl stop` would,
	// or it would start each relay StopRelay stops again.
	var once sync.Once
	sb.stopService = func() { once.Do(func() { close(stop) }) }
	go func() {
		defer close(done)
		for {
			began := time.Now()
			cmd := exec.Command(sb.Terma, "relay", "run", "--service", "--idle", "0", "--quiet")
			cmd.Env, cmd.Dir = sb.termaEnv(), sb.Repo
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				cmd.Stdout, cmd.Stderr = f, f
			}
			err = cmd.Run()
			if f != nil {
				_ = f.Close()
			}
			if err == nil {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(manager(time.Since(began))):
			}
		}
	}()
	t.Cleanup(func() { sb.StopRelay(); <-done })
	sb.logRelayOnFailure(logPath)
	if !sb.waitRelay(10 * time.Second) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("the service's relay never came up on %s:\n%s", sb.relayAddr, data)
	}
}

// recordHarnesses sets the default profile's chosen agents.
func (sb *Sandbox) recordHarnesses(agents []string) {
	path := filepath.Join(sb.TermaConfig, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		sb.T.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		sb.T.Fatal(err)
	}
	file["profiles"].(map[string]any)["default"].(map[string]any)["harnesses"] = agents
	data, _ = json.Marshal(file)
	sb.writeAbs(path, string(data))
}

// StartRelay runs `terma relay run` in the background, never idling out.
func (sb *Sandbox) StartRelay() {
	t := sb.T
	t.Helper()
	cmd := exec.Command(sb.Terma, "relay", "run", "--idle", "0", "--quiet")
	// Every drop is logged with its reason, sender and claim; a failing test prints it.
	cmd.Env = append(sb.termaEnv(), "TERMA_RELAY_DEBUG=1")
	cmd.Dir = sb.Repo
	logPath := filepath.Join(sb.Dir, "relay.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()
	sb.logRelayOnFailure(logPath)
	if !sb.waitRelay(10 * time.Second) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("relay never came up on %s:\n%s", sb.relayAddr, data)
	}
}

// logHookRelayOnFailure is logRelayOnFailure for the relays hooks start, which keep their
// own log in the relay's directory, every one after the last; what they print goes to
// daemon.log beside it.
func (sb *Sandbox) logHookRelayOnFailure() {
	dir := filepath.Join(sb.TermaConfig, "relay")
	sb.logRelayOnFailure(filepath.Join(dir, "activity.log"))
	sb.logRelayOnFailure(filepath.Join(dir, "daemon.log"))
}

// logRelayOnFailure logs, when the test fails, the relay log at logPath, the claims, and
// when each hook ran: which hook claimed a session, and how long after its first export.
func (sb *Sandbox) logRelayOnFailure(logPath string) {
	t := sb.T
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		data, _ := os.ReadFile(logPath)
		t.Logf("relay log:\n%s", tail(string(data), 4000))
		// The last relay's exit counters, and the one's before it.
		for _, name := range []string{"stats-prev.json", "stats.json"} {
			data, _ := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", name))
			t.Logf("%s: %s", name, strings.Join(strings.Fields(string(data)), " "))
		}
		claims, _ := filepath.Glob(filepath.Join(sb.TermaConfig, "relay", "claims", "*.json"))
		for _, c := range claims {
			data, _ := os.ReadFile(c)
			t.Logf("claim %s: %s", filepath.Base(c), data)
		}
		hooks, _ := os.ReadDir(sb.payloadDir())
		for _, h := range hooks {
			name := h.Name()
			if ns, event, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-"); ok {
				if n, err := strconv.ParseInt(ns, 10, 64); err == nil {
					t.Logf("hook %s at %s", event, time.Unix(0, n).Format("15:04:05.000"))
				}
			}
		}
	})
}

// WaitRelayCount waits until the running relay's counters under prefix sum to at least
// n, and reports whether they did within timeout.
func (sb *Sandbox) WaitRelayCount(prefix string, n int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if c, ok := sb.relayStatsLive(); ok && sum(c, prefix) >= n {
			return true
		}
	}
	return false
}

// WaitRelaySettled waits until the running relay has forwarded or dropped everything
// it received, and reports whether it did within timeout.
func (sb *Sandbox) WaitRelaySettled(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if c, ok := sb.relayStatsLive(); ok && sum(c, "received.") > 0 && sum(c, "received.") <= sum(c, "forwarded.")+sum(c, "dropped.") {
			return true
		}
	}
	return false
}

// waitRelay reports whether the relay answers on its address within timeout.
func (sb *Sandbox) waitRelay(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, ok := sb.relayStatsLive(); ok {
			return true
		}
	}
	return false
}

// StopRelay stops the relays running, whoever started them, one waiting to take over
// included once it has, and waits for each to write its stats and let go of its lock.
func (sb *Sandbox) StopRelay() {
	if sb.stopService != nil {
		sb.stopService()
	}
	dir := filepath.Join(sb.TermaConfig, "relay")
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		pid := recordedPID(dir)
		if pid <= 0 {
			if !relayLocked(dir) {
				return
			}
			continue // a relay going, or one about to record itself
		}
		if syscall.Kill(pid, syscall.SIGTERM) != nil {
			return // a record no relay left behind: killed, so nothing holds the lock
		}
		for until := time.Now().Add(10 * time.Second); recordedPID(dir) == pid && time.Now().Before(until); {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// relayLocked reports whether a relay holds dir's lock.
func relayLocked(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// recordedPID is the pid the running relay recorded in its directory dir, 0 when none runs.
func recordedPID(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
	if err != nil {
		return 0
	}
	var rec struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(data, &rec)
	return rec.PID
}

func (sb *Sandbox) relayToken() string {
	data, _ := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "token"))
	return strings.TrimSpace(string(data))
}

func (sb *Sandbox) relayStatsLive() (map[string]int, bool) {
	req, _ := http.NewRequest(http.MethodGet, "http://"+sb.relayAddr+"/stats", nil)
	req.Header.Set("Authorization", "Bearer "+sb.relayToken())
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var snap struct {
		Counters map[string]int `json:"counters"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&snap) != nil {
		return nil, false
	}
	return snap.Counters, true
}

// RelayStats are the relay's counters: live while it runs, else those it left.
func (sb *Sandbox) RelayStats() map[string]int {
	if c, ok := sb.relayStatsLive(); ok {
		return c
	}
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "stats.json"))
	if err != nil {
		return nil
	}
	var snap struct {
		Counters map[string]int `json:"counters"`
	}
	_ = json.Unmarshal(data, &snap)
	return snap.Counters
}

// sum adds the counters whose names start with prefix.
func sum(c map[string]int, prefix string) int {
	n := 0
	for k, v := range c {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}
