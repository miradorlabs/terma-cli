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
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		data, _ := os.ReadFile(logPath)
		t.Logf("relay log:\n%s", tail(string(data), 4000))
		// When each claim was written, and when each hook ran: which hook claimed a
		// session, and how long after its first export.
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
	if !sb.waitRelay(10 * time.Second) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("relay never came up on %s:\n%s", sb.relayAddr, data)
	}
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

// StopRelay stops the relay, whoever started it, and waits for it to write its stats.
func (sb *Sandbox) StopRelay() {
	dir := filepath.Join(sb.TermaConfig, "relay")
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "pid")); os.IsNotExist(err) {
			return
		}
	}
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
