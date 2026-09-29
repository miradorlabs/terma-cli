package live

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The relay (docs/RELAY.md) end to end. UseRelay sets a sandbox up the way `terma setup`
// and `terma install` leave a developer's machine: every agent's global export points at
// terma's loopback relay, the relay runs (`terma relay serve`, which launchd or systemd
// would run), and the receiver stands in for Terma upstream. What reaches the receiver
// under each project's key is what the relay delivered to that project.

// Keys and projects a relay sandbox holds. The bound repository is the sandbox's own
// (ProjectID, liveKey); the machine project is where the relay sends everything that
// is in no bound repository or names no session.
const (
	machineProject = "proj_machine"
	machineKey     = "ter_srv_11111111111111111111111111111111"
	otherProject   = "proj_other"
	otherKey       = "ter_srv_22222222222222222222222222222222"
)

// RelayOptions shape a relay scenario.
type RelayOptions struct {
	// Content lets prompts and tool content through for every project (the relay's
	// per-project policy, relay/policy/<project>.json); without it both are withheld.
	Content bool
	// NoStart leaves the relay stopped; the scenario starts it.
	NoStart bool
}

// sandboxRelay is the relay a sandbox runs.
type sandboxRelay struct {
	port  int
	token string
	api   *httptest.Server
	mu    sync.Mutex
	cmd   *exec.Cmd
	out   bytes.Buffer
	last  RelayHealth
}

// RelayHealth is the relay's /healthz.
type RelayHealth struct {
	Version      string         `json:"version"`
	Backlog      int            `json:"backlog"`
	HeldProjects []string       `json:"held_projects"`
	LastError    string         `json:"last_error"`
	Counters     map[string]int `json:"counters"`
}

// projectKeys are the projects this sandbox's machine holds keys for.
func (sb *Sandbox) projectKeys() map[string]string {
	return map[string]string{sb.ProjectID: liveKey, machineProject: machineKey, otherProject: otherKey}
}

// UseRelay sets the sandbox up as setup and install do on a developer's machine, with
// the relay. It must run before any agent does.
//
// Nothing here reaches the developer's own machine: the service managers are stand-ins
// on the sandbox's PATH (install would otherwise replace the developer's own
// ai.terma.relay launch agent), the relay listens on a port of its own (not the
// developer's 14318), and every file is under the sandbox.
func (sb *Sandbox) UseRelay(o RelayOptions) {
	t := sb.T
	t.Helper()
	if sb.Mode != Isolated {
		t.Fatal("UseRelay needs an isolated sandbox: install would write the developer's own home")
	}
	r := &sandboxRelay{}
	sb.relay = r
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	tok := make([]byte, 32)
	_, _ = rand.Read(tok)
	r.token = hex.EncodeToString(tok)
	cfg, _ := json.Marshal(map[string]any{"port": r.port, "token": r.token})
	sb.writeAbs(filepath.Join(sb.TermaConfig, "relay", "relay.json"), string(cfg)+"\n")

	sb.stubServiceManagers()

	// The API gateway's /v1/identity, the one request a server-key install makes: the
	// key names its own project.
	projectOf := map[string]string{}
	for p, k := range sb.projectKeys() {
		projectOf["Bearer "+k] = p
	}
	r.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p, ok := projectOf[req.Header.Get("Authorization")]
		if req.URL.Path != "/v1/identity" || !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"project_id":%q,"organization_id":"org_live"}`, p)
	}))
	t.Cleanup(r.api.Close)

	// Keys a signed-in developer's installs would have minted, for every project the
	// scenarios use, each delivering to the receiver.
	spool, perAgent, hosts := map[string]string{}, map[string]string{}, map[string]any{}
	for p, k := range sb.projectKeys() {
		spool[p], perAgent[p] = k, k
		hosts[p] = map[string]string{"otlp": sb.Receiver.URL(), "api": r.api.URL}
	}
	keys, _ := json.Marshal(map[string]any{"keys": spool, "harness_keys": map[string]any{"claude": perAgent, "codex": perAgent}, "hosts": hosts})
	sb.writeAbs(filepath.Join(sb.TermaConfig, "keys.json"), string(keys)+"\n")

	// The machine project: the first install on a machine with no machine project makes
	// the repository's its own, as setup's picker would; everything unbound goes there.
	home := filepath.Join(sb.Dir, "machine-home")
	sb.newRepo(home)
	sb.installRelayed(home, machineProject, machineKey)
	// The sandbox repository, installed with the agents now; its telemetry is already
	// configured machine-wide, so this binds it and wires its hooks.
	sb.installRelayed(sb.Repo, sb.ProjectID, liveKey)
	sb.checkRelayConfigured()
	// Each project's content policy, and the machine default, written after the installs
	// so the scenario's choice is what the relay reads.
	for _, p := range []string{"@machine", sb.ProjectID, machineProject, otherProject} {
		sb.SetContentPolicy(p, o.Content)
	}

	t.Cleanup(sb.StopRelay)
	if !o.NoStart {
		sb.StartRelay()
	}
}

// installRelayed runs `terma install` in dir with a server key, as a developer's
// install of a repository bound to project does.
func (sb *Sandbox) installRelayed(dir, project, key string) {
	sb.T.Helper()
	sb.termaWith([]string{"TERMA_API_KEY=" + key, "TERMA_API_URL=" + sb.relay.api.URL}, dir,
		"install", "--project", project, "--harness", "claude,codex", "--adapters", "claude,codex", "--yes", "--no-browser", "--no-doctor")
}

// InstallOther binds a second repository to another project, as a developer who works
// in two does, and returns it.
func (sb *Sandbox) InstallOther() string {
	sb.T.Helper()
	dir := filepath.Join(sb.Dir, "other")
	sb.newRepo(dir)
	sb.installRelayed(dir, otherProject, otherKey)
	return dir
}

// newRepo makes an empty committed git repository at dir.
func (sb *Sandbox) newRepo(dir string) {
	sb.T.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		sb.T.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "live@terma.test"},
		{"config", "user.name", "Terma Live"}, {"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		sb.gitIn(dir, args...)
	}
}

// SetContentPolicy writes project's content policy for the relay (relay/policy/<project>.json;
// "@machine" is the machine default).
func (sb *Sandbox) SetContentPolicy(project string, content bool) {
	sb.T.Helper()
	p, _ := json.Marshal(map[string]bool{"include_prompts": content, "include_tool_content": content})
	sb.writeAbs(filepath.Join(sb.TermaConfig, "relay", "policy", project+".json"), string(p)+"\n")
}

// checkRelayConfigured fails unless install left every agent exporting to the relay, the
// way setup does: the scenario proves nothing about the relay otherwise.
func (sb *Sandbox) checkRelayConfigured() {
	t := sb.T
	t.Helper()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", sb.relay.port)
	claude, _ := os.ReadFile(filepath.Join(sb.ClaudeConfig, "settings.json"))
	codex, _ := os.ReadFile(filepath.Join(sb.CodexHome, "config.toml"))
	if !bytes.Contains(claude, []byte(endpoint)) || !bytes.Contains(codex, []byte(endpoint)) {
		t.Fatalf("install did not point the agents at the relay (%s):\nClaude:\n%s\nCodex:\n%s", endpoint, claude, codex)
	}
	if bytes.Contains(codex, []byte("mirador.project.id")) {
		t.Fatalf("Codex's global configuration names a project; the relay decides it:\n%s", codex)
	}
}

// stubServiceManagers puts launchctl and systemctl stand-ins first on the sandbox's
// PATH: install asks the service manager to run the relay, and the real one would load
// a job in the developer's own session. The stand-ins record what they were asked and
// report no job loaded.
func (sb *Sandbox) stubServiceManagers() {
	dir := strings.TrimSuffix(sb.binDir(), ":")
	log := filepath.Join(sb.Dir, "service-manager.log")
	for name, body := range map[string]string{
		"launchctl": `[ "$1" = print ] && exit 113` + "\nexit 0\n",
		"systemctl": `case "$2" in is-active) echo inactive; exit 3;; esac` + "\nexit 0\n",
	} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> \"" + log + "\"\n" + body
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			sb.T.Fatal(err)
		}
	}
}

// StartRelay runs `terma relay serve` in the background, as the service manager would.
func (sb *Sandbox) StartRelay() {
	t := sb.T
	t.Helper()
	r := sb.relay
	r.mu.Lock()
	cmd := exec.Command(sb.Terma, "relay", "serve")
	cmd.Env = sb.termaEnv()
	cmd.Dir = sb.Dir
	r.out.Reset()
	cmd.Stdout, cmd.Stderr = &r.out, &r.out
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		t.Fatalf("start relay: %v", err)
	}
	r.cmd = cmd
	r.mu.Unlock()
	if !sb.waitRelay(10 * time.Second) {
		t.Fatalf("relay never answered on 127.0.0.1:%d:\n%s\nlog:\n%s", r.port, r.out.String(), sb.relayLog())
	}
}

// waitRelay reports whether the relay answers within timeout.
func (sb *Sandbox) waitRelay(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, ok := sb.relayHealthLive(); ok {
			return true
		}
	}
	return false
}

// StopRelay stops the relay gracefully (SIGTERM, as a service manager does), keeping its
// last health for RelayStats.
func (sb *Sandbox) StopRelay() { sb.stopRelay(syscall.SIGTERM) }

// KillRelay kills the relay outright: a crash, with nothing flushed.
func (sb *Sandbox) KillRelay() { sb.stopRelay(syscall.SIGKILL) }

func (sb *Sandbox) stopRelay(sig syscall.Signal) {
	r := sb.relay
	if r == nil {
		return
	}
	if h, ok := sb.relayHealthLive(); ok {
		r.last = h
	}
	r.mu.Lock()
	cmd := r.cmd
	r.cmd = nil
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func (sb *Sandbox) relayHealthLive() (RelayHealth, bool) {
	r := sb.relay
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", r.port), nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return RelayHealth{}, false
	}
	defer resp.Body.Close()
	var h RelayHealth
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&h) != nil {
		return RelayHealth{}, false
	}
	return h, true
}

// RelayStats is the relay's health: live while it runs, else the last it reported.
func (sb *Sandbox) RelayStats() RelayHealth {
	if h, ok := sb.relayHealthLive(); ok {
		return h
	}
	return sb.relay.last
}

// relayLog is the relay's own log (relay/relay.log).
func (sb *Sandbox) relayLog() string {
	data, _ := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "relay.log"))
	return string(data)
}

// relayDrained waits until the relay has nothing left to deliver.
func (sb *Sandbox) relayDrained(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if h, ok := sb.relayHealthLive(); ok && h.Backlog == 0 {
			return true
		}
	}
	return false
}
